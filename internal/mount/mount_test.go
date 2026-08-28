package mount

import (
	"context"
	"errors"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()
	input := strings.Join([]string{
		`35 24 0:31 / /mnt/infinidysk rw,nosuid,nodev shared:18 - fuse.rclone infinidysk: rw,user_id=0`,
		`36 24 0:32 / /mnt/with\040space rw - ext4 /dev/sda1 rw`,
	}, "\n")
	entries, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d", len(entries))
	}
	if entries[0].Filesystem != "fuse.rclone" || entries[0].Source != "infinidysk:" {
		t.Fatalf("entries[0] = %+v", entries[0])
	}
	if entries[1].Mountpoint != "/mnt/with space" {
		t.Fatalf("entries[1].Mountpoint = %q", entries[1].Mountpoint)
	}
}

func TestParseRejectsMalformedRecord(t *testing.T) {
	t.Parallel()
	if _, err := Parse(strings.NewReader("not mountinfo")); err == nil {
		t.Fatal("Parse() error = nil")
	}
}

func TestRepairerEscalatesAndVerifies(t *testing.T) {
	t.Parallel()
	table := &fakeTable{entries: []Entry{{Mountpoint: "/mnt/x", Filesystem: ownedFilesystem}}}
	unmounter := &fakeUnmounter{table: table, succeedOn: syscall.MNT_DETACH}
	repairer := Repairer{Mountpoint: "/mnt/x", Table: table, Unmounter: unmounter}
	if err := repairer.Clear(context.Background()); err != nil {
		t.Fatalf("Clear() error = %v", err)
	}
	want := []int{0, syscall.MNT_FORCE, syscall.MNT_DETACH}
	if !slices.Equal(unmounter.flags, want) {
		t.Fatalf("flags = %v, want %v", unmounter.flags, want)
	}
}

func TestRepairerClearsStackedMounts(t *testing.T) {
	t.Parallel()
	table := &fakeTable{entries: []Entry{
		{Mountpoint: "/mnt/x", Filesystem: ownedFilesystem},
		{Mountpoint: "/mnt/x", Filesystem: ownedFilesystem},
	}}
	unmounter := &fakeUnmounter{table: table, succeedOn: 0}
	repairer := Repairer{Mountpoint: "/mnt/x", Table: table, Unmounter: unmounter}
	if err := repairer.Clear(context.Background()); err != nil {
		t.Fatalf("Clear() error = %v", err)
	}
	if len(unmounter.flags) != 2 {
		t.Fatalf("unmount calls = %d, want 2", len(unmounter.flags))
	}
}

func TestRepairerRefusesForeignFilesystem(t *testing.T) {
	t.Parallel()
	table := &fakeTable{entries: []Entry{{Mountpoint: "/mnt/x", Filesystem: "ext4"}}}
	unmounter := &fakeUnmounter{table: table}
	repairer := Repairer{Mountpoint: "/mnt/x", Table: table, Unmounter: unmounter}
	if err := repairer.Clear(context.Background()); err == nil {
		t.Fatal("Clear() error = nil")
	}
	if len(unmounter.flags) != 0 {
		t.Fatalf("unmount calls = %d", len(unmounter.flags))
	}
}

func TestRepairerReportsMissingCapability(t *testing.T) {
	t.Parallel()
	table := &fakeTable{entries: []Entry{{Mountpoint: "/mnt/x", Filesystem: ownedFilesystem}}}
	unmounter := &fakeUnmounter{table: table, err: syscall.EPERM}
	repairer := Repairer{Mountpoint: "/mnt/x", Table: table, Unmounter: unmounter}
	err := repairer.Clear(context.Background())
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("Clear() error = %v, want EPERM", err)
	}
}

type fakeTable struct {
	entries []Entry
}

func (t *fakeTable) Entries() ([]Entry, error) {
	return slices.Clone(t.entries), nil
}

type fakeUnmounter struct {
	table     *fakeTable
	succeedOn int
	err       error
	flags     []int
}

func (u *fakeUnmounter) Unmount(_ string, flags int) error {
	u.flags = append(u.flags, flags)
	if u.err != nil {
		return u.err
	}
	if flags != u.succeedOn {
		return syscall.EBUSY
	}
	if len(u.table.entries) > 0 {
		u.table.entries = u.table.entries[1:]
	}
	return nil
}
