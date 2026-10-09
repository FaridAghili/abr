package manager

import (
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"abr/internal/config"
	"abr/internal/ports"
)

func restorePair(t *testing.T) (config.Config, ports.Registry) {
	t.Helper()
	c := config.Default()
	c.Apps = []config.App{testApp("demo")}
	r := ports.Empty()
	if err := r.Ensure(c.Apps[0], c.Ports, nil, func(int) error { return nil }); err != nil {
		t.Fatal(err)
	}
	return c, r
}

func TestRestoreSnapshotRefusesConflictsAndExistingState(t *testing.T) {
	c, r := restorePair(t)
	m := testManager(t)
	m.Probe = func(int) error { return ports.ErrOccupied }
	if err := m.RestoreSnapshot(c, r); err == nil || !errors.Is(err, ports.ErrOccupied) {
		t.Fatal("occupied saved port accepted", err)
	}
	if _, err := os.Stat(m.ConfigPath); !os.IsNotExist(err) {
		t.Fatal("port conflict installed config")
	}
	m.Probe = func(int) error { return nil }
	if err := m.RestoreSnapshot(c, r); err != nil {
		t.Fatal(err)
	}
	actual, registry, err := m.Snapshot()
	if err != nil || len(actual.Apps) != 1 || len(registry.Assignments) != len(r.Assignments) {
		t.Fatal("lost restore settings", err)
	}
	if err := m.RestoreSnapshot(c, r); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatal("restored over existing apps", err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	m = testManager(t)
	m.Probe = ports.CheckAvailable
	r.Assignments[0].Port = listener.Addr().(*net.TCPAddr).Port
	if err := m.RestoreSnapshot(c, r); !errors.Is(err, ports.ErrOccupied) {
		t.Fatal("real listener conflict not detected", err)
	}
}

func TestBackupSnapshotBlocksConcurrentWriters(t *testing.T) {
	m := testManager(t)
	if _, err := m.Register(testApp("demo"), nil); err != nil {
		t.Fatal(err)
	}
	captured := make(chan struct{})
	release := make(chan struct{})
	backupDone := make(chan error, 1)
	go func() {
		backupDone <- m.WithSnapshot(func(c config.Config, r ports.Registry) error {
			if len(c.Apps) != 1 {
				return errors.New("unexpected snapshot")
			}
			close(captured)
			<-release
			return CheckReservations(c, r)
		})
	}()
	<-captured
	writerDone := make(chan error, 1)
	go func() { _, err := m.Register(testApp("second"), nil); writerDone <- err }()
	select {
	case err := <-writerDone:
		close(release)
		t.Fatalf("writer escaped backup locks: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-backupDone; err != nil {
		t.Fatal(err)
	}
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
}
