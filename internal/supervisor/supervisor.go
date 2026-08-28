// Package supervisor owns the foreground rclone process and its mount lifecycle.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const forcedCleanupTimeout = 5 * time.Second

// MountManager reports and repairs the configured rclone mount.
type MountManager interface {
	Mounted() (bool, error)
	Clear(ctx context.Context) error
}

// Process is a supervised rclone process group.
type Process interface {
	PID() int
	Wait() <-chan error
	Signal(signal syscall.Signal) error
	Kill() error
}

// Starter creates a foreground rclone process.
type Starter interface {
	Start() (Process, error)
}

// CommandStarter starts an OS process in a dedicated process group.
type CommandStarter struct {
	Path   string
	Args   []string
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Start launches the configured command.
func (s CommandStarter) Start() (Process, error) {
	cmd := exec.Command(s.Path, s.Args...)
	cmd.Env = s.Env
	cmd.Stdin = s.Stdin
	cmd.Stdout = s.Stdout
	cmd.Stderr = s.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start rclone: %w", err)
	}
	process := &commandProcess{cmd: cmd, wait: make(chan error, 1)}
	go func() {
		process.wait <- cmd.Wait()
		close(process.wait)
	}()
	return process, nil
}

type commandProcess struct {
	cmd  *exec.Cmd
	wait chan error
}

func (p *commandProcess) PID() int                      { return p.cmd.Process.Pid }
func (p *commandProcess) Wait() <-chan error            { return p.wait }
func (p *commandProcess) Kill() error                   { return signalGroup(p.PID(), syscall.SIGKILL) }
func (p *commandProcess) Signal(s syscall.Signal) error { return signalGroup(p.PID(), s) }

func signalGroup(pid int, signal syscall.Signal) error {
	err := syscall.Kill(-pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// Supervisor configures the rclone restart and shutdown state machine.
type Supervisor struct {
	Mountpoint      string
	Mount           MountManager
	Starter         Starter
	Logger          *slog.Logger
	ShutdownTimeout time.Duration
	PollInterval    time.Duration
	MinRestartDelay time.Duration
	MaxRestartDelay time.Duration
	StableWindow    time.Duration
}

// Run supervises rclone until the context is canceled or a termination signal is
// received. It returns only after the final mount cleanup completes.
func (s Supervisor) Run(ctx context.Context, signals <-chan os.Signal) error {
	s.applyDefaults()
	delay := s.MinRestartDelay

	for {
		if err := s.Mount.Clear(ctx); err != nil {
			return fmt.Errorf("clear mount before rclone start: %w", err)
		}
		if err := os.MkdirAll(s.Mountpoint, 0o755); err != nil {
			return fmt.Errorf("create mountpoint: %w", err)
		}

		process, err := s.Starter.Start()
		if err != nil {
			s.Logger.Error("failed to start rclone", "error", err, "restart_in", delay)
			terminate, waitErr := s.waitToRestart(ctx, signals, delay)
			if waitErr != nil {
				return waitErr
			}
			if terminate {
				return s.Mount.Clear(ctx)
			}
			delay = nextDelay(delay, s.MaxRestartDelay)
			continue
		}

		s.Logger.Info("started rclone", "pid", process.PID())
		started := time.Now()
		mountSeen := false
		ticker := time.NewTicker(s.PollInterval)
		restart := false

		for !restart {
			select {
			case <-ctx.Done():
				ticker.Stop()
				_, stopErr := s.stopProcess(ctx, process, syscall.SIGTERM, signals, true)
				return errors.Join(ctx.Err(), stopErr)
			case err := <-process.Wait():
				ticker.Stop()
				if err != nil {
					s.Logger.Error("rclone exited", "pid", process.PID(), "error", err)
				} else {
					s.Logger.Warn("rclone exited", "pid", process.PID())
				}
				restart = true
			case signal := <-signals:
				unixSignal, ok := signal.(syscall.Signal)
				if !ok {
					continue
				}
				if unixSignal == syscall.SIGHUP {
					if err := process.Signal(unixSignal); err != nil {
						s.Logger.Warn("failed to forward SIGHUP", "error", err)
					}
					continue
				}
				if unixSignal != syscall.SIGINT && unixSignal != syscall.SIGTERM {
					continue
				}
				ticker.Stop()
				_, err := s.stopProcess(ctx, process, unixSignal, signals, true)
				return err
			case <-ticker.C:
				mounted, err := s.Mount.Mounted()
				if err != nil {
					ticker.Stop()
					_, stopErr := s.stopProcess(ctx, process, syscall.SIGTERM, signals, false)
					return errors.Join(fmt.Errorf("inspect supervised mount: %w", err), stopErr)
				}
				if mounted {
					mountSeen = true
					continue
				}
				if mountSeen {
					ticker.Stop()
					s.Logger.Error("rclone mount disappeared", "pid", process.PID())
					terminate, stopErr := s.stopProcess(ctx, process, syscall.SIGTERM, signals, false)
					if stopErr != nil {
						return stopErr
					}
					if terminate {
						return nil
					}
					restart = true
				}
			}
		}

		if err := s.Mount.Clear(ctx); err != nil {
			return fmt.Errorf("clear mount after rclone exit: %w", err)
		}
		if mountSeen && time.Since(started) >= s.StableWindow {
			delay = s.MinRestartDelay
		}
		s.Logger.Info("restarting rclone", "restart_in", delay)
		terminate, err := s.waitToRestart(ctx, signals, delay)
		if err != nil {
			return err
		}
		if terminate {
			return s.Mount.Clear(ctx)
		}
		delay = nextDelay(delay, s.MaxRestartDelay)
	}
}

func (s *Supervisor) applyDefaults() {
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	if s.ShutdownTimeout <= 0 {
		s.ShutdownTimeout = 30 * time.Second
	}
	if s.PollInterval <= 0 {
		s.PollInterval = time.Second
	}
	if s.MinRestartDelay <= 0 {
		s.MinRestartDelay = time.Second
	}
	if s.MaxRestartDelay <= 0 {
		s.MaxRestartDelay = time.Minute
	}
	if s.StableWindow <= 0 {
		s.StableWindow = 5 * time.Minute
	}
}

func (s Supervisor) waitToRestart(ctx context.Context, signals <-chan os.Signal, delay time.Duration) (bool, error) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-timer.C:
			return false, nil
		case signal := <-signals:
			unixSignal, ok := signal.(syscall.Signal)
			if !ok || unixSignal == syscall.SIGHUP {
				continue
			}
			if unixSignal == syscall.SIGINT || unixSignal == syscall.SIGTERM {
				return true, nil
			}
		}
	}
}

func (s Supervisor) stopProcess(ctx context.Context, process Process, signal syscall.Signal, signals <-chan os.Signal, external bool) (bool, error) {
	s.Logger.Info("stopping rclone", "pid", process.PID(), "signal", signal)
	if err := process.Signal(signal); err != nil {
		s.Logger.Warn("failed to signal rclone", "pid", process.PID(), "error", err)
	}
	timer := time.NewTimer(s.ShutdownTimeout)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			external = true
			return external, s.forceStop(ctx, process)
		case <-process.Wait():
			return external, s.Mount.Clear(ctx)
		case <-timer.C:
			s.Logger.Warn("rclone shutdown timed out", "pid", process.PID())
			return external, s.forceStop(ctx, process)
		case next := <-signals:
			unixSignal, ok := next.(syscall.Signal)
			if !ok {
				continue
			}
			if unixSignal == syscall.SIGHUP {
				_ = process.Signal(unixSignal)
				continue
			}
			if unixSignal == syscall.SIGINT || unixSignal == syscall.SIGTERM {
				s.Logger.Warn("received another termination signal; escalating", "signal", unixSignal)
				return true, s.forceStop(ctx, process)
			}
		}
	}
}

func (s Supervisor) forceStop(ctx context.Context, process Process) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), forcedCleanupTimeout)
	cleanupErr := s.Mount.Clear(cleanupCtx)
	cancel()
	if err := process.Kill(); err != nil {
		return errors.Join(cleanupErr, fmt.Errorf("kill rclone process group: %w", err))
	}

	waitTimer := time.NewTimer(forcedCleanupTimeout)
	defer waitTimer.Stop()
	select {
	case <-process.Wait():
	case <-waitTimer.C:
		return errors.Join(cleanupErr, errors.New("rclone did not exit after SIGKILL"))
	}
	finalErr := s.Mount.Clear(context.WithoutCancel(ctx))
	return errors.Join(cleanupErr, finalErr)
}

func nextDelay(current, maximum time.Duration) time.Duration {
	if current >= maximum/2 {
		return maximum
	}
	return min(current*2, maximum)
}

// BinaryPath resolves the manager-owned executable path below a binary directory.
func BinaryPath(binDirectory string) string {
	return filepath.Join(binDirectory, "rclone")
}

// ManifestPath resolves the manager-owned validation manifest path.
func ManifestPath(dataDirectory string) string {
	return filepath.Join(dataDirectory, "rclone.json")
}
