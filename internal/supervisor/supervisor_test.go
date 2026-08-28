package supervisor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestSupervisorRestartsAfterChildExit(t *testing.T) {
	t.Parallel()
	mount := newFakeMount()
	starter := newFakeStarter(mount, true)
	signals := make(chan os.Signal, 2)
	done := runSupervisor(t, mount, starter, signals)

	first := awaitProcess(t, starter.started)
	first.exit(errors.New("failed"))
	second := awaitProcess(t, starter.started)
	signals <- syscall.SIGTERM
	if err := awaitResult(t, done); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !second.signaledWith(syscall.SIGTERM) {
		t.Fatal("second process did not receive SIGTERM")
	}
	if mount.clearCount() < 3 {
		t.Fatalf("Clear() calls = %d, want at least 3", mount.clearCount())
	}
}

func TestSupervisorRestartsWhenMountDisappears(t *testing.T) {
	t.Parallel()
	mount := newFakeMount()
	starter := newFakeStarter(mount, true)
	signals := make(chan os.Signal, 2)
	done := runSupervisor(t, mount, starter, signals)

	first := awaitProcess(t, starter.started)
	awaitMountState(t, mount.observed, true)
	mount.setMounted(false)
	second := awaitProcess(t, starter.started)
	if !first.signaledWith(syscall.SIGTERM) {
		t.Fatal("first process did not receive SIGTERM after mount disappeared")
	}
	signals <- syscall.SIGTERM
	if err := awaitResult(t, done); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !second.signaledWith(syscall.SIGTERM) {
		t.Fatal("second process did not receive SIGTERM")
	}
}

func TestSupervisorEscalatesStuckShutdown(t *testing.T) {
	t.Parallel()
	mount := newFakeMount()
	starter := newFakeStarter(mount, false)
	signals := make(chan os.Signal, 2)
	done := runSupervisor(t, mount, starter, signals)

	process := awaitProcess(t, starter.started)
	signals <- syscall.SIGTERM
	if err := awaitResult(t, done); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !process.killed() {
		t.Fatal("process was not killed after timeout")
	}
}

func TestNextDelay(t *testing.T) {
	t.Parallel()
	if got := nextDelay(time.Second, time.Minute); got != 2*time.Second {
		t.Fatalf("nextDelay() = %v", got)
	}
	if got := nextDelay(40*time.Second, time.Minute); got != time.Minute {
		t.Fatalf("nextDelay() = %v", got)
	}
}

func TestRuntimePathsUseConfiguredDirectories(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	binDirectory := filepath.Join(root, "bin")
	dataDirectory := filepath.Join(root, "data")
	if got, want := BinaryPath(binDirectory), filepath.Join(binDirectory, "rclone"); got != want {
		t.Fatalf("BinaryPath() = %q, want %q", got, want)
	}
	if got, want := ManifestPath(dataDirectory), filepath.Join(dataDirectory, "rclone.json"); got != want {
		t.Fatalf("ManifestPath() = %q, want %q", got, want)
	}
}

func runSupervisor(t *testing.T, mount *fakeMount, starter *fakeStarter, signals chan os.Signal) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	manager := Supervisor{
		Mountpoint:      filepath.Join(t.TempDir(), "mount"),
		Mount:           mount,
		Starter:         starter,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		ShutdownTimeout: time.Millisecond,
		PollInterval:    time.Millisecond,
		MinRestartDelay: time.Nanosecond,
		MaxRestartDelay: time.Microsecond,
		StableWindow:    time.Hour,
	}
	go func() {
		done <- manager.Run(context.Background(), signals)
	}()
	return done
}

func awaitProcess(t *testing.T, started <-chan *fakeProcess) *fakeProcess {
	t.Helper()
	select {
	case process := <-started:
		return process
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for process start")
		return nil
	}
}

func awaitResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for supervisor")
		return nil
	}
}

func awaitMountState(t *testing.T, observed <-chan bool, want bool) {
	t.Helper()
	for {
		select {
		case got := <-observed:
			if got == want {
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for mounted state %t", want)
		}
	}
}

type fakeMount struct {
	mu       sync.Mutex
	mounted  bool
	clears   int
	observed chan bool
}

func newFakeMount() *fakeMount {
	return &fakeMount{observed: make(chan bool, 16)}
}

func (m *fakeMount) Mounted() (bool, error) {
	m.mu.Lock()
	mounted := m.mounted
	m.mu.Unlock()
	m.observed <- mounted
	return mounted, nil
}

func (m *fakeMount) Clear(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clears++
	m.mounted = false
	return nil
}

func (m *fakeMount) setMounted(value bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mounted = value
}

func (m *fakeMount) clearCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.clears
}

type fakeStarter struct {
	mount    *fakeMount
	autoExit bool
	started  chan *fakeProcess
}

func newFakeStarter(mount *fakeMount, autoExit bool) *fakeStarter {
	return &fakeStarter{mount: mount, autoExit: autoExit, started: make(chan *fakeProcess, 8)}
}

func (s *fakeStarter) Start() (Process, error) {
	process := &fakeProcess{pid: 42, wait: make(chan error, 1), autoExit: s.autoExit}
	s.mount.setMounted(true)
	s.started <- process
	return process, nil
}

type fakeProcess struct {
	mu        sync.Mutex
	pid       int
	wait      chan error
	once      sync.Once
	autoExit  bool
	signals   []syscall.Signal
	wasKilled bool
}

func (p *fakeProcess) PID() int           { return p.pid }
func (p *fakeProcess) Wait() <-chan error { return p.wait }

func (p *fakeProcess) Signal(signal syscall.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, signal)
	autoExit := p.autoExit
	p.mu.Unlock()
	if autoExit && (signal == syscall.SIGINT || signal == syscall.SIGTERM) {
		p.exit(nil)
	}
	return nil
}

func (p *fakeProcess) Kill() error {
	p.mu.Lock()
	p.wasKilled = true
	p.mu.Unlock()
	p.exit(nil)
	return nil
}

func (p *fakeProcess) exit(err error) {
	p.once.Do(func() {
		p.wait <- err
		close(p.wait)
	})
}

func (p *fakeProcess) signaledWith(signal syscall.Signal) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Contains(p.signals, signal)
}

func (p *fakeProcess) killed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.wasKilled
}
