// Package mount inspects and repairs Linux mount attachments without accessing
// the potentially broken mounted filesystem.
package mount

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const ownedFilesystem = "fuse.rclone"

// Entry is the subset of a Linux mountinfo record needed by the manager.
type Entry struct {
	ID         int
	Mountpoint string
	Filesystem string
	Source     string
}

// Table reads the active mount table.
type Table interface {
	Entries() ([]Entry, error)
}

// Unmounter removes the topmost attachment at a path.
type Unmounter interface {
	Unmount(path string, flags int) error
}

// ProcTable reads Linux mount information from procfs.
type ProcTable struct {
	Path string
}

// Entries returns the current mount namespace's attachments.
func (t ProcTable) Entries() ([]Entry, error) {
	path := t.Path
	if path == "" {
		path = "/proc/self/mountinfo"
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open mount table: %w", err)
	}
	defer func() { _ = file.Close() }()

	entries, err := Parse(file)
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// Parse reads Linux /proc/*/mountinfo records.
func Parse(reader io.Reader) ([]Entry, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var entries []Entry
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		separator := -1
		for index, field := range fields {
			if field == "-" {
				separator = index
				break
			}
		}
		if len(fields) < 10 || separator < 6 || separator+2 >= len(fields) {
			return nil, fmt.Errorf("parse mount table: malformed record %q", scanner.Text())
		}
		id, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, fmt.Errorf("parse mount ID %q: %w", fields[0], err)
		}
		entries = append(entries, Entry{
			ID:         id,
			Mountpoint: unescape(fields[4]),
			Filesystem: fields[separator+1],
			Source:     unescape(fields[separator+2]),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read mount table: %w", err)
	}
	return entries, nil
}

// SyscallUnmounter invokes Linux umount2 directly.
type SyscallUnmounter struct{}

// Unmount removes the topmost mount using the supplied Linux flags.
func (SyscallUnmounter) Unmount(path string, flags int) error {
	return syscall.Unmount(path, flags)
}

// Repairer safely clears rclone-owned attachments from one exact mountpoint.
type Repairer struct {
	Mountpoint string
	Table      Table
	Unmounter  Unmounter
	RetryDelay time.Duration
}

// Mounted reports whether the configured path has an rclone FUSE attachment.
// It rejects any unrelated filesystem at the same path.
func (r Repairer) Mounted() (bool, error) {
	entries, err := r.Table.Entries()
	if err != nil {
		return false, err
	}
	found := false
	for _, entry := range entries {
		if entry.Mountpoint != r.Mountpoint {
			continue
		}
		if entry.Filesystem != ownedFilesystem {
			return false, fmt.Errorf("refusing to unmount %s filesystem at %s", entry.Filesystem, r.Mountpoint)
		}
		found = true
	}
	return found, nil
}

// Clear escalates through normal, forced, and lazy unmounts until mountinfo no
// longer contains the manager-owned attachment.
func (r Repairer) Clear(ctx context.Context) error {
	delay := r.RetryDelay
	if delay <= 0 {
		delay = time.Second
	}

	for {
		mounted, err := r.Mounted()
		if err != nil {
			return err
		}
		if !mounted {
			return nil
		}

		var attempts []error
		unmounted := false
		for _, flags := range []int{0, syscall.MNT_FORCE, syscall.MNT_DETACH} {
			err = r.Unmounter.Unmount(r.Mountpoint, flags)
			if err == nil {
				unmounted = true
				break
			}
			if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
				return fmt.Errorf("unmount %s: missing CAP_SYS_ADMIN: %w", r.Mountpoint, err)
			}
			attempts = append(attempts, fmt.Errorf("flags %#x: %w", flags, err))
		}
		if unmounted {
			continue
		}

		mounted, statusErr := r.Mounted()
		if statusErr != nil {
			return statusErr
		}
		if !mounted {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("clear mount %s: %w: %w", r.Mountpoint, errors.Join(attempts...), ctx.Err())
		case <-time.After(delay):
		}
	}
}

func unescape(value string) string {
	replacer := strings.NewReplacer(
		`\040`, " ",
		`\011`, "\t",
		`\012`, "\n",
		`\134`, `\`,
	)
	return replacer.Replace(value)
}
