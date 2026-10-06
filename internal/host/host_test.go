package host

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sites-manager/internal/config"
	"sites-manager/internal/manager"
	"sites-manager/internal/ports"
	"sites-manager/internal/services"
)

type testExit int

func (e testExit) Error() string { return fmt.Sprintf("exit %d", e) }
func (e testExit) ExitCode() int { return int(e) }

type fakeRunner struct {
	calls            []Command
	users            map[string]string
	database         bool
	fail             func(Command) error
	dirty, processes bool
}

func (r *fakeRunner) Run(c Command) ([]byte, error) {
	r.calls = append(r.calls, c)
	if r.fail != nil {
		if err := r.fail(c); err != nil {
			return nil, err
		}
	}
	switch c.Name {
	case "getent":
		if entry, ok := r.users[c.Args[len(c.Args)-1]]; ok {
			return []byte(entry), nil
		}
		return nil, testExit(2)
	case "useradd":
		user := c.Args[len(c.Args)-1]
		var home, comment string
		for i, arg := range c.Args {
			if arg == "--home-dir" {
				home = c.Args[i+1]
			}
			if arg == "--comment" {
				comment = c.Args[i+1]
			}
		}
		r.users[user] = fmt.Sprintf("%s:x:991:991:%s:%s:/usr/sbin/nologin", user, comment, home)
	case "userdel":
		delete(r.users, c.Args[0])
	case "pgrep":
		if r.processes {
			return []byte("999\n"), nil
		}
		return nil, testExit(1)
	case "mysql":
		if strings.HasPrefix(string(c.Input), "SELECT") {
			if r.database {
				return []byte("2\n"), nil
			}
			return []byte("0\n"), nil
		}
		r.database = true
	case "systemctl":
		if len(c.Args) > 0 && c.Args[0] == "show" {
			return []byte("loaded\n"), nil
		}
	case "runuser":
		text := strings.Join(c.Args, " ")
		if strings.Contains(text, "git status") && r.dirty {
			return []byte(" M tracked-file\n"), nil
		}
		if strings.Contains(text, "git rev-parse HEAD") {
			return []byte(strings.Repeat("a", 40) + "\n"), nil
		}
	}
	return nil, nil
}

func fixture(t *testing.T) (Host, *fakeRunner, *bytes.Buffer, config.App) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{users: map[string]string{}}
	var out bytes.Buffer
	h := Host{Manager: manager.Manager{ConfigPath: filepath.Join(root, "state/config.toml"), StateDir: filepath.Join(root, "state"), Probe: func(int) error { return nil }}, TemplatesDir: "../../templates", AppsDir: filepath.Join(root, "apps"), Output: &out, Runner: runner, root: root, check: func() error { return nil }}
	a := config.App{Name: "app", User: "sites-app", Directory: filepath.Join(h.AppsDir, "app"), Type: "laravel", Domain: "app.localhost", Web: config.Web{Driver: "fpm"}}
	for _, dir := range []string{"public", "storage/app/public", "bootstrap/cache", "vendor"} {
		if err := os.MkdirAll(filepath.Join(a.Directory, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"artisan", "vendor/autoload.php", "public/index.php", "composer.lock", "package.json", "package-lock.json"} {
		if err := os.WriteFile(filepath.Join(a.Directory, file), []byte("{}\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(a.Directory, ".env"), []byte("APP_KEY=existing-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return h, runner, &out, a
}

func TestRegisterDatabaseIsPrivateStableAndScoped(t *testing.T) {
	h, runner, out, a := fixture(t)
	a.Database.Enabled = true
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	data, err := h.read(h.credentialsPath(a.Name))
	if err != nil {
		t.Fatal(err)
	}
	var c credentials
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	if !c.Ready || len(c.Password) != 68 || strings.Contains(out.String(), c.Password) {
		t.Fatal("credential leak or invalid credentials")
	}
	info, err := os.Stat(h.credentialsEnvPath(a.Name))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("credential permissions: %v %v", info, err)
	}
	var sql string
	for _, command := range runner.calls {
		if command.Name == "mysql" && strings.Contains(string(command.Input), "GRANT") {
			sql = string(command.Input)
			if !command.Private || strings.Contains(strings.Join(command.Args, " "), c.Password) {
				t.Fatal("SQL password exposed")
			}
		}
	}
	if !strings.Contains(sql, "ON `sites_app`.*") || strings.Contains(sql, "*.*") || strings.Contains(sql, "GRANT OPTION") {
		t.Fatal(sql)
	}
	if err := h.Database(a.Name, false); err != nil {
		t.Fatal(err)
	}
	data2, _ := h.read(h.credentialsPath(a.Name))
	if !bytes.Equal(data, data2) {
		t.Fatal("credentials changed on retry")
	}
	if err := h.Database(a.Name, true); err != nil || !strings.Contains(out.String(), "DB_PASSWORD="+c.Password) {
		t.Fatalf("explicit --show failed: %v", err)
	}
}

func TestSQLFailureKeepsCredentialsForRetry(t *testing.T) {
	h, r, _, a := fixture(t)
	a.Database.Enabled = true
	fail := true
	r.fail = func(c Command) error {
		if c.Name == "mysql" && strings.Contains(string(c.Input), "CREATE DATABASE") && fail {
			return testExit(1)
		}
		return nil
	}
	if _, err := h.Register(a, nil); err == nil {
		t.Fatal("SQL failure ignored")
	}
	saved, err := h.read(h.credentialsPath(a.Name))
	if err != nil {
		t.Fatal(err)
	}
	var pending credentials
	_ = json.Unmarshal(saved, &pending)
	fail = false
	if err := h.Database(a.Name, false); err != nil {
		t.Fatal(err)
	}
	ready, _ := h.read(h.credentialsPath(a.Name))
	var c credentials
	_ = json.Unmarshal(ready, &c)
	if c.Password != pending.Password || !c.Ready {
		t.Fatal("partial database retry rotated password")
	}
}

func TestRefuseExistingAccountAndDatabase(t *testing.T) {
	for _, kind := range []string{"user", "database"} {
		t.Run(kind, func(t *testing.T) {
			h, r, _, a := fixture(t)
			if kind == "user" {
				r.users[a.User] = "sites-app:x:1000:1000:unrelated:/home/owner:/bin/bash"
			} else {
				a.Database.Enabled = true
				r.database = true
			}
			if _, err := h.Register(a, nil); err == nil {
				t.Fatal("adopted unrelated resource")
			}
			if kind == "user" {
				for _, c := range r.calls {
					if c.Name == "chown" || c.Name == "useradd" {
						t.Fatal("changed unrelated account/project")
					}
				}
			}
		})
	}
}

func TestValidationFailureRestoresFilesAndManifest(t *testing.T) {
	h, r, _, a := fixture(t)
	registry, err := h.Register(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := services.Render(a, registry, h.TemplatesDir, h.Manager.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	old := manifest{Version: 1, App: a.Name, User: a.User, FPM: true, Enabled: true}
	for _, f := range p.Files {
		old.Files = append(old.Files, f.Path)
		if err := h.write(f.Path, f.Data, f.Mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.saveManifest(old); err != nil {
		t.Fatal(err)
	}
	r.fail = func(c Command) error {
		if c.Name == "caddy" && c.Args[0] == "validate" {
			return testExit(1)
		}
		return nil
	}
	p.Files[1].Data = []byte(services.Marker + "invalid caddy configuration")
	if err := h.apply(a, registry, p); err == nil {
		t.Fatal("configuration failure ignored")
	}
	for _, f := range p.Files {
		data, _ := h.read(f.Path)
		if bytes.Equal(data, []byte(services.Marker+"invalid caddy configuration")) {
			t.Fatal("failed configuration retained")
		}
	}
	m, _, err := h.loadManifest(a)
	if err != nil || !m.Enabled {
		t.Fatalf("old manifest not restored: %+v %v", m, err)
	}
}

func TestUnmanagedFilesAndCorruptManifestRefused(t *testing.T) {
	h, _, _, a := fixture(t)
	registry, err := h.Register(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := services.Render(a, registry, h.TemplatesDir, h.Manager.StateDir)
	if err := h.write(p.Files[1].Path, []byte("unrelated config"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := h.apply(a, registry, p); err == nil {
		t.Fatal("overwrote unmanaged config")
	}
	if err := h.write(h.manifestPath(a.Name), []byte(`{"Version":1,"App":"app","User":"sites-app","Files":["/etc/passwd"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.Remove(a.Name); err == nil {
		t.Fatal("accepted unowned manifest path")
	}
}

func TestRemovalStopsBeforeUserDeletionAndPreservesData(t *testing.T) {
	h, r, _, a := fixture(t)
	a.Database.Enabled = true
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	unit := "sites-app-queue@1.service"
	path := "/etc/systemd/system/sites-app-queue@.service"
	if err := h.write(path, []byte(services.Marker+"[Service]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := h.saveManifest(manifest{Version: 1, App: a.Name, User: a.User, Files: []string{path}, Units: []string{unit}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	r.processes = true
	if err := h.Remove(a.Name); err == nil {
		t.Fatal("removed user with running processes")
	}
	c, err := config.Load(h.Manager.ConfigPath)
	if err != nil || len(c.Apps) != 1 {
		t.Fatal("failed removal unregistered app")
	}
	r.processes = false
	if err := h.Remove(a.Name); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(a.Directory, ".env"), h.credentialsPath(a.Name), h.credentialsEnvPath(a.Name)} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("data removed: %s", path)
		}
	}
	stop, del := -1, -1
	for i, c := range r.calls {
		if c.Name == "systemctl" && len(c.Args) > 0 && c.Args[0] == "stop" {
			stop = i
		}
		if c.Name == "userdel" {
			del = i
			if len(c.Args) != 1 {
				t.Fatal("user files would be deleted")
			}
		}
	}
	if stop < 0 || del <= stop {
		t.Fatal("user deleted before services stopped")
	}
	c, err = config.Load(h.Manager.ConfigPath)
	if err != nil || len(c.Apps) != 0 {
		t.Fatal("removal did not unregister")
	}
}

func TestDeploymentStopsOnFailureAndRecordsResult(t *testing.T) {
	h, r, out, a := fixture(t)
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	r.fail = func(c Command) error {
		if c.Name == "runuser" && strings.Contains(strings.Join(c.Args, " "), "composer install") {
			return testExit(7)
		}
		return nil
	}
	if err := h.Deploy([]string{a.Name}, DeployOptions{NoPull: true}); err == nil {
		t.Fatal("failed install reported success")
	}
	if strings.Contains(out.String(), "Deployed app") {
		t.Fatal("false success printed")
	}
	for _, c := range r.calls {
		if c.Name == "runuser" && strings.Contains(strings.Join(c.Args, " "), "artisan migrate") {
			t.Fatal("migration ran after dependency failure")
		}
	}
	paths, _ := filepath.Glob(filepath.Join(h.Manager.StateDir, "deployments/app/*.json"))
	if len(paths) != 1 {
		t.Fatalf("missing history: %v", paths)
	}
	data, _ := os.ReadFile(paths[0])
	if !bytes.Contains(data, []byte(`"Success": false`)) {
		t.Fatal(string(data))
	}
	info, _ := os.Stat(strings.TrimSuffix(paths[0], ".json") + ".log")
	if info.Mode().Perm() != 0600 {
		t.Fatal("log not private")
	}
}

func TestDryRunNeverExecutesOrWrites(t *testing.T) {
	h, r, out, a := fixture(t)
	h.DryRun = true
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 0 {
		t.Fatal("dry-run executed commands")
	}
	if _, err := os.Stat(h.Manager.ConfigPath); !os.IsNotExist(err) {
		t.Fatal("dry-run saved config")
	}
	if err := h.Setup(SetupOptions{RoadRunnerVersion: DefaultRoadRunnerVersion}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Redis") && !strings.Contains(out.String(), "redis-server") {
		t.Fatal("default Redis missing")
	}
	if len(r.calls) != 0 {
		t.Fatal("setup preview executed commands")
	}
	if _, err := os.Stat(h.Manager.StateDir); !os.IsNotExist(err) {
		t.Fatal("preview created state")
	}
}

func TestPrivateRunnerDoesNotPrintInputOrErrors(t *testing.T) {
	var out bytes.Buffer
	r := ExecRunner{Output: &out}
	_, err := r.Run(Command{Name: "sh", Args: []string{"-c", "cat; exit 7"}, Input: []byte("example-secret"), Private: true})
	if err == nil || out.Len() != 0 || strings.Contains(err.Error(), "example-secret") {
		t.Fatal("private command leaked output")
	}
}

func TestRunnerKeepsDiagnosticsOutOfQueryResults(t *testing.T) {
	var out bytes.Buffer
	r := ExecRunner{Output: &out}
	data, err := r.Run(Command{Name: "sh", Args: []string{"-c", "printf '0\\n'; printf 'diagnostic warning\\n' >&2"}, Private: true})
	if err != nil || string(data) != "0\n" || out.Len() != 0 {
		t.Fatalf("diagnostic polluted query: %q %v", data, err)
	}
	h, runner, _, a := fixture(t)
	_, err = h.asUser(a, map[string]string{"APP_ENV": "production"}, true, "git", "status", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(runner.calls[0].Args, " "), "env -i HOME=") {
		t.Fatal("app command inherited root environment")
	}
}

func TestProjectEnvSymlinkCannotChangeOutsidePermissions(t *testing.T) {
	h, _, _, a := fixture(t)
	outside := filepath.Join(h.root, "outside")
	if err := os.WriteFile(outside, []byte("unchanged"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(a.Directory, ".env")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(a.Directory, ".env")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Register(a, nil); err == nil {
		t.Fatal("accepted secret symlink")
	}
	info, err := os.Stat(outside)
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatal("changed symlink target")
	}
}

func TestRoadRunnerDigestAndArchiveEntryValidation(t *testing.T) {
	makeArchive := func(name string, kind byte) []byte {
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		tr := tar.NewWriter(gz)
		data := []byte("\x7fELFtest-executable")
		if kind != tar.TypeReg {
			data = nil
		}
		if err := tr.WriteHeader(&tar.Header{Name: name, Typeflag: kind, Size: int64(len(data)), Mode: 0755, Linkname: "/etc/passwd"}); err != nil {
			t.Fatal(err)
		}
		_, _ = tr.Write(data)
		_ = tr.Close()
		_ = gz.Close()
		return b.Bytes()
	}
	archive := makeArchive("release/rr", tar.TypeReg)
	sum := sha256.Sum256(archive)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if binary, err := verifiedRoadRunner(archive, digest); err != nil || !bytes.HasPrefix(binary, []byte("\x7fELF")) {
		t.Fatalf("%v", err)
	}
	if _, err := verifiedRoadRunner(archive, "sha256:bad"); err == nil {
		t.Fatal("bad digest accepted")
	}
	archive = makeArchive("rr", tar.TypeSymlink)
	sum = sha256.Sum256(archive)
	if _, err := verifiedRoadRunner(archive, "sha256:"+hex.EncodeToString(sum[:])); err == nil {
		t.Fatal("archive symlink accepted")
	}
}

func TestFPMReadyChecksActualSocket(t *testing.T) {
	h, _, _, a := fixture(t)
	short, err := os.MkdirTemp("", "sites-socket-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(short)
	// Unix socket paths have a small platform-dependent maximum length.
	h.root = short
	path := h.path("/run/php/sites-app.sock")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := h.ready(a, ports.Empty(), manifest{FPM: true}); err != nil {
		t.Fatal(err)
	}
}
