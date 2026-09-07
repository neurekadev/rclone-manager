// Package config loads and validates rclone-manager's environment contract.
package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	// DefaultMountpoint is the mount path used when RCLONE_MOUNTPOINT is unset.
	DefaultMountpoint = "/mnt/rclone"
	// DefaultVersion tracks the latest stable rclone release.
	DefaultVersion = "latest"
	// DefaultShutdownTimeout bounds graceful rclone shutdown.
	DefaultShutdownTimeout = 30 * time.Second
	// BinDir contains manager-downloaded executables.
	BinDir = "/app/bin"
	// DataDir contains manager-owned persistent data and validation metadata.
	DataDir = "/app/data"
)

var stableVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// Config contains the validated manager settings.
type Config struct {
	Remote          string
	Version         string
	Mountpoint      string
	ShutdownTimeout time.Duration
	DaemonIgnored   bool
}

// Load parses the supplied process environment. Later duplicate keys win.
func Load(environ []string) (Config, error) {
	values := make(map[string]string, len(environ))
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}

	cfg := Config{
		Remote:          values["RCLONE_REMOTE"],
		Version:         valueOr(values, "RCLONE_VERSION", DefaultVersion),
		Mountpoint:      valueOr(values, "RCLONE_MOUNTPOINT", DefaultMountpoint),
		ShutdownTimeout: DefaultShutdownTimeout,
	}

	if strings.TrimSpace(cfg.Remote) == "" {
		return Config{}, fmt.Errorf("RCLONE_REMOTE is required")
	}

	version, err := NormalizeVersion(cfg.Version)
	if err != nil {
		return Config{}, err
	}
	cfg.Version = version

	if strings.ContainsRune(cfg.Mountpoint, '\x00') {
		return Config{}, fmt.Errorf("RCLONE_MOUNTPOINT contains a null byte")
	}
	cleanMountpoint := filepath.Clean(cfg.Mountpoint)
	if !filepath.IsAbs(cleanMountpoint) {
		return Config{}, fmt.Errorf("RCLONE_MOUNTPOINT must be absolute")
	}
	if cleanMountpoint == string(filepath.Separator) {
		return Config{}, fmt.Errorf("RCLONE_MOUNTPOINT must not be the filesystem root")
	}
	cfg.Mountpoint = cleanMountpoint

	if raw := values["RCLONE_MANAGER_SHUTDOWN_TIMEOUT"]; raw != "" {
		timeout, parseErr := time.ParseDuration(raw)
		if parseErr != nil {
			return Config{}, fmt.Errorf("parse RCLONE_MANAGER_SHUTDOWN_TIMEOUT: %w", parseErr)
		}
		if timeout <= 0 {
			return Config{}, fmt.Errorf("RCLONE_MANAGER_SHUTDOWN_TIMEOUT must be positive")
		}
		cfg.ShutdownTimeout = timeout
	}

	_, cfg.DaemonIgnored = values["RCLONE_DAEMON"]
	return cfg, nil
}

// NormalizeVersion validates a stable rclone version or the latest selector.
func NormalizeVersion(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, DefaultVersion) {
		return DefaultVersion, nil
	}
	value = strings.TrimPrefix(value, "v")
	if !stableVersion.MatchString(value) {
		return "", fmt.Errorf("RCLONE_VERSION must be latest or an exact stable version such as 1.74.2")
	}
	return value, nil
}

// RcloneEnvironment removes manager-owned and unsupported variables before exec.
func RcloneEnvironment(environ []string) []string {
	filtered := make([]string, 0, len(environ))
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || managerVariable(key) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

// SanitizedEnvironment removes every rclone option while probing the binary.
func SanitizedEnvironment(environ []string) []string {
	filtered := make([]string, 0, len(environ))
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(key, "RCLONE_") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func managerVariable(key string) bool {
	return key == "RCLONE_REMOTE" ||
		key == "RCLONE_VERSION" ||
		key == "RCLONE_MOUNTPOINT" ||
		key == "RCLONE_DAEMON" ||
		strings.HasPrefix(key, "RCLONE_MANAGER_")
}

func valueOr(values map[string]string, key, fallback string) string {
	if value := values[key]; value != "" {
		return value
	}
	return fallback
}
