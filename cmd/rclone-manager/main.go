package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/neurekadev/rclone-manager/internal/config"
	"github.com/neurekadev/rclone-manager/internal/install"
	"github.com/neurekadev/rclone-manager/internal/mount"
	"github.com/neurekadev/rclone-manager/internal/supervisor"
	"github.com/neurekadev/rclone-manager/internal/telemetry"
)

var (
	gitTag  = "dev"
	gitHash = "unknown"
)

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Printf("rclone-manager %s (%s)\n", gitTag, gitHash)
		return
	}

	verification := len(os.Args) == 2 && os.Args[1] == "telemetry-test"
	level := slog.LevelInfo
	if verification {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	os.Exit(execute(logger, verification))
}

func execute(logger *slog.Logger, verification bool) (code int) {
	reporter, telemetryErr := telemetry.New(telemetry.Options{
		DataDir:      config.DataDir,
		Release:      gitTag,
		Environment:  deploymentEnvironment(),
		Platform:     runtime.GOOS + "-" + runtime.GOARCH,
		Logger:       logger,
		Verification: verification,
	})
	if telemetryErr != nil {
		logger.Warn("Beacon telemetry is unavailable", "error", telemetryErr)
	}
	defer func() { reporter.Close(code == 0) }()
	defer func() {
		if recovered := recover(); recovered != nil {
			code = 2
			reporter.Recover(recovered)
			panic(recovered)
		}
	}()

	if verification {
		if !reporter.Enabled() {
			logger.Error("Beacon telemetry verification requires telemetry to be enabled")
			return 1
		}
		reporter.Start()
		if !reporter.CaptureException(errors.New("controlled Beacon error reporting verification")) {
			logger.Error("failed to queue controlled Beacon error event")
			return 1
		}
		if !reporter.FlushErrors() {
			logger.Error("controlled Beacon error event flush timed out")
			return 1
		}
		logger.Info("sent controlled handled Beacon error event")
		return 0
	}

	if err := run(logger, reporter); err != nil {
		reporter.CaptureException(err)
		logger.Error("rclone-manager stopped", "error", err)
		return 1
	}
	return 0
}

func run(logger *slog.Logger, reporter *telemetry.Reporter) error {
	cfg, err := config.Load(os.Environ())
	if err != nil {
		return err
	}
	if cfg.DaemonIgnored {
		logger.Warn("RCLONE_DAEMON is unsupported and will be ignored; rclone must run in the foreground")
	}

	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	preflightCtx, stopPreflight := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopPreflight()

	repairer := mount.Repairer{
		Mountpoint: cfg.Mountpoint,
		Table:      mount.ProcTable{},
		Unmounter:  mount.SyscallUnmounter{},
	}
	if err := repairer.Clear(preflightCtx); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("repair stale mount: %w", err)
	}

	binaryPath := supervisor.BinaryPath(config.BinDir)
	installer := install.Installer{
		BinaryPath:   binaryPath,
		ManifestPath: supervisor.ManifestPath(config.DataDir),
	}
	result, err := ensureRclone(preflightCtx, logger, installer, cfg.Version)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	if result.Downloaded {
		logger.Info("installed rclone", "version", result.Version, "path", binaryPath)
	} else if result.UsedCached {
		logger.Warn("using validated cached rclone", "version", result.Version, "error", result.FallbackErr)
	} else {
		logger.Info("using installed rclone", "version", result.Version, "path", binaryPath)
	}

	stopPreflight()
	rcloneEnvironment := config.RcloneEnvironment(os.Environ())
	manager := supervisor.Supervisor{
		Mountpoint: cfg.Mountpoint,
		Mount:      repairer,
		Starter: supervisor.CommandStarter{
			Path:   binaryPath,
			Args:   []string{"mount", cfg.Remote, cfg.Mountpoint},
			Env:    rcloneEnvironment,
			Stdin:  os.Stdin,
			Stdout: os.Stdout,
			Stderr: os.Stderr,
		},
		Logger:          logger,
		ShutdownTimeout: cfg.ShutdownTimeout,
	}
	reporter.Start()
	return manager.Run(context.Background(), signals)
}

func deploymentEnvironment() string {
	if environment := strings.TrimSpace(os.Getenv("RCLONE_MANAGER_ENVIRONMENT")); environment != "" {
		return environment
	}
	return "production"
}

func ensureRclone(ctx context.Context, logger *slog.Logger, installer install.Installer, version string) (install.Result, error) {
	delay := time.Second
	for {
		result, err := installer.Ensure(ctx, version)
		if err == nil {
			return result, nil
		}
		if install.IsPermanent(err) {
			return install.Result{}, err
		}
		logger.Error("rclone installation failed; retrying", "error", err, "retry_in", delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return install.Result{}, ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, time.Minute)
	}
}
