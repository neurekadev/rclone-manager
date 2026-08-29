package config

import (
	"slices"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := Load([]string{"RCLONE_REMOTE=infinidysk:"})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Remote != "infinidysk:" || cfg.Version != DefaultVersion || cfg.Mountpoint != DefaultMountpoint {
		t.Fatalf("Load() = %+v", cfg)
	}
	if cfg.ShutdownTimeout != DefaultShutdownTimeout {
		t.Fatalf("ShutdownTimeout = %v, want %v", cfg.ShutdownTimeout, DefaultShutdownTimeout)
	}
}

func TestLoadValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  []string
	}{
		{name: "missing remote"},
		{name: "invalid version", env: []string{"RCLONE_REMOTE=x:", "RCLONE_VERSION=1.2"}},
		{name: "relative mountpoint", env: []string{"RCLONE_REMOTE=x:", "RCLONE_MOUNTPOINT=mnt/x"}},
		{name: "root mountpoint", env: []string{"RCLONE_REMOTE=x:", "RCLONE_MOUNTPOINT=/"}},
		{name: "zero timeout", env: []string{"RCLONE_REMOTE=x:", "RCLONE_MANAGER_SHUTDOWN_TIMEOUT=0s"}},
		{name: "bad timeout", env: []string{"RCLONE_REMOTE=x:", "RCLONE_MANAGER_SHUTDOWN_TIMEOUT=soon"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Load(test.env); err == nil {
				t.Fatal("Load() error = nil")
			}
		})
	}
}

func TestLoadNormalizesValues(t *testing.T) {
	t.Parallel()
	cfg, err := Load([]string{
		"RCLONE_REMOTE=x:",
		"RCLONE_VERSION=v1.74.2",
		"RCLONE_MOUNTPOINT=/mnt/other/../data/",
		"RCLONE_MANAGER_SHUTDOWN_TIMEOUT=45s",
		"RCLONE_DAEMON=true",
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Version != "1.74.2" || cfg.Mountpoint != "/mnt/data" || cfg.ShutdownTimeout != 45*time.Second || !cfg.DaemonIgnored {
		t.Fatalf("Load() = %+v", cfg)
	}
}

func TestRcloneEnvironment(t *testing.T) {
	t.Parallel()
	environ := []string{
		"PATH=/usr/bin",
		"RCLONE_REMOTE=x:",
		"RCLONE_VERSION=latest",
		"RCLONE_MOUNTPOINT=/mnt/x",
		"RCLONE_MANAGER_SHUTDOWN_TIMEOUT=30s",
		"RCLONE_DAEMON=true",
		"TELEMETRY=false",
		"RCLONE_VFS_CACHE_MODE=full",
		"RCLONE_CONFIG_X_TYPE=sftp",
	}
	want := []string{"PATH=/usr/bin", "RCLONE_VFS_CACHE_MODE=full", "RCLONE_CONFIG_X_TYPE=sftp"}
	if got := RcloneEnvironment(environ); !slices.Equal(got, want) {
		t.Fatalf("RcloneEnvironment() = %q, want %q", got, want)
	}
}

func TestSanitizedEnvironment(t *testing.T) {
	t.Parallel()
	environ := []string{"PATH=/usr/bin", "TZ=UTC", "RCLONE_CONFIG=/secret", "RCLONE_RC=true"}
	want := []string{"PATH=/usr/bin", "TZ=UTC"}
	if got := SanitizedEnvironment(environ); !slices.Equal(got, want) {
		t.Fatalf("SanitizedEnvironment() = %q, want %q", got, want)
	}
}
