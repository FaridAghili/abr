package ports

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"sites-manager/internal/config"
)

func app() config.App {
	return config.App{Name: "app", Directory: "/srv/app", User: "app", Type: "laravel", Domain: "app.test", Web: config.Web{Driver: "octane"}}
}
func free(int) error { return nil }

func TestStableAssignmentsAndDisabledReservations(t *testing.T) {
	r := Empty()
	a := app()
	probe := func(p int) error {
		if p == 10000 {
			return ErrOccupied
		}
		return nil
	}
	if err := r.Ensure(a, config.Range{First: 10000, Last: 10002}, nil, probe); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.Lookup("app", "octane-http"); p != 10001 {
		t.Fatalf("got %d", p)
	}
	path := filepath.Join(t.TempDir(), "ports.json")
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
	saved, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// An occupied saved port and a changed range must never cause reassignment.
	occupied := func(int) error { return ErrOccupied }
	if err := saved.Ensure(a, config.Range{First: 20000, Last: 20001}, nil, occupied); err != nil {
		t.Fatal(err)
	}
	a.Web.Driver = "fpm"
	if err := saved.Ensure(a, config.Range{First: 20000, Last: 20001}, nil, occupied); err != nil {
		t.Fatal(err)
	}
	a.Web.Driver = "octane"
	if err := saved.Ensure(a, config.Range{First: 10000, Last: 10002}, nil, occupied); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r, saved) {
		t.Fatalf("assignments changed: %+v vs %+v", r, saved)
	}
}

func TestImportsAndAllocationFailure(t *testing.T) {
	r := Empty()
	a := app()
	if err := r.Ensure(a, config.Range{First: 10000, Last: 10001}, map[string]int{"roadrunner-rpc": 10000}, free); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.Lookup("app", "octane-http"); p != 10001 {
		t.Fatalf("automatic allocation stole import: %d", p)
	}
	before := Registry{Version: 1, Assignments: append([]Assignment{}, r.Assignments...)}
	b := a
	b.Name = "other"
	for _, imports := range []map[string]int{{"octane-http": 10000}, {"octane-http": 80}, {"nuxt-http": 20000}, {"octane-http": 20000, "roadrunner-rpc": 20000}} {
		if err := r.Ensure(b, config.Range{First: 10000, Last: 10001}, imports, free); err == nil {
			t.Fatalf("accepted imports %v", imports)
		}
		if !reflect.DeepEqual(r, before) {
			t.Fatal("failed allocation changed registry")
		}
	}
	if err := r.Ensure(b, config.Range{First: 10000, Last: 10001}, nil, free); err == nil {
		t.Fatal("accepted exhausted range")
	}
	if err := r.Ensure(b, config.Range{First: 20000, Last: 20001}, nil, func(int) error { return errors.New("permission denied") }); err == nil {
		t.Fatal("ignored probe error")
	}
	if err := r.Ensure(b, config.Range{First: 20000, Last: 20001}, map[string]int{"octane-http": 20000}, func(int) error { return ErrOccupied }); !errors.Is(err, ErrOccupied) {
		t.Fatalf("got %v", err)
	}
	if !reflect.DeepEqual(r, before) {
		t.Fatal("failed allocation changed registry")
	}
}

func TestCorruptRegistryRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ports.json")
	for _, bad := range []string{
		"", "{", "null", "{}", `{"version":2,"assignments":[]}`, `{"version":1,"assignments":null}`, `{"version":1,"assignments":[],"extra":1}`, `{"version":1,"assignments":[]} {}`,
		`{"version":1,"assignments":[{"app":"a","purpose":"octane-http","port":10000},{"app":"b","purpose":"nuxt-http","port":10000}]}`,
		`{"version":1,"assignments":[{"app":"a","purpose":"octane-http","port":10000},{"app":"a","purpose":"octane-http","port":10001}]}`,
		`{"version":1,"assignments":[{"app":"a","purpose":"fpm","port":10000}]}`,
	} {
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("accepted corrupt registry %s", bad)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != bad {
			t.Fatal("corrupt state changed")
		}
	}
}

func TestListenerConflicts(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		t.Run(network, func(t *testing.T) {
			address := "127.0.0.1:0"
			if network == "tcp6" {
				address = "[::1]:0"
			}
			listener, err := net.Listen(network, address)
			if err != nil {
				if network == "tcp6" {
					t.Skipf("IPv6 unavailable: %v", err)
				}
				t.Fatal(err)
			}
			defer listener.Close()
			port := listener.Addr().(*net.TCPAddr).Port
			if err := CheckAvailable(port); !errors.Is(err, ErrOccupied) {
				t.Fatalf("listener not detected: %v", err)
			}
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			if err := CheckAvailable(port); err != nil {
				t.Fatalf("closed port unavailable: %v", err)
			}
		})
	}
}

func TestLinuxClosedConnectionsDoNotReservePorts(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux TIME_WAIT bind semantics")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	server.Close() // Server-initiated close leaves this listening port in TIME_WAIT.
	client.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, err := client.Read(b[:]); err == nil {
		t.Fatal("connection was not closed")
	}
	client.Close()
	listener.Close()
	if err := CheckAvailable(port); err != nil {
		t.Fatalf("closed connection blocks reuse after service shutdown: %v", err)
	}
}
