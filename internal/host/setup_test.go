package host

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"abr"
)

const testSSHSettings = "authorizedkeysfile .ssh/authorized_keys\nport 2222\npubkeyauthentication yes\nauthenticationmethods publickey\npasswordauthentication no\nkbdinteractiveauthentication no\npermitrootlogin without-password\npermitemptypasswords no\n"

type setupRunner struct {
	*fakeRunner
	ssh, version, insecure, redis, hostnameAccounts string
}

func (r *setupRunner) Run(c Command) ([]byte, error) {
	data, err := r.fakeRunner.Run(c)
	if err != nil {
		return data, err
	}
	if c.Name == "/usr/sbin/sshd" && c.Args[0] == "-T" {
		return []byte(r.ssh), nil
	}
	if c.Name == "ssh-keygen" {
		return (ExecRunner{}).Run(c)
	}
	if c.Name == "systemctl" && slices.Contains(c.Args, "--property=Listen") {
		return []byte("[::]:2200 (Stream)"), nil
	}
	if c.Name == "caddy" && c.Args[0] == "adapt" {
		return []byte(`{}`), nil
	}
	if c.Name == "redis-cli" {
		if slices.Contains(c.Args, "SET") {
			return []byte("OK\n"), nil
		}
		if slices.Contains(c.Args, "INFO") {
			return []byte("aof_enabled:1\naof_rewrite_in_progress:0\naof_rewrite_scheduled:0\naof_last_bgrewrite_status:ok\naof_last_write_status:ok\n"), nil
		}
		return []byte(r.redis), nil
	}
	if c.Name == "mysql" {
		sql := string(c.Input)
		switch {
		case strings.Contains(sql, "SELECT VERSION()"):
			return []byte(r.version), nil
		case strings.Contains(sql, "WHERE User='' OR"):
			return []byte(r.insecure), nil
		case strings.Contains(sql, "Host REGEXP"):
			return []byte(r.hostnameAccounts), nil
		case strings.Contains(sql, "SELECT (@@bind_address"):
			return []byte("1\n"), nil
		case strings.Contains(sql, "mysql.component"):
			return []byte("0\n"), nil
		}
	}
	return data, nil
}

func setupFixture(t *testing.T) (Host, *setupRunner) {
	t.Helper()
	h, r, _, _ := fixture(t)
	wr := &setupRunner{fakeRunner: r, ssh: testSSHSettings, version: "8.4.11\n", insecure: "0\n", hostnameAccounts: "0\n", redis: "bind\n127.0.0.1 -::1\nprotected-mode\nyes\n"}
	h.Runner = wr
	wr.users["ubuntu"] = fmt.Sprintf("ubuntu:x:%d:%d:admin:/home/ubuntu:/bin/bash", os.Getuid(), os.Getgid())
	if err := os.MkdirAll(h.path("/home/ubuntu/.ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := h.write("/home/ubuntu/.ssh/authorized_keys", []byte(strings.TrimPrefix(githubHostKey, "github.com ")), 0600); err != nil {
		t.Fatal(err)
	}
	return h, wr
}

func TestAPTRepositoryUsesTrustedLocalKeys(t *testing.T) {
	h, _, _, _ := fixture(t)
	// Hosted runners make /usr/share recursively world-writable. Local keys
	// must not be written there, and setup must leave those unrelated keys alone.
	shared := h.path("/usr/share/keyrings")
	if err := os.MkdirAll(shared, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0777); err != nil {
		t.Fatal(err)
	}
	key := []byte("test repository key")
	source := "Types: deb\nURIs: https://example.invalid\nSuites: stable\nComponents: main\n"
	for range 2 {
		if err := h.writeAPTRepository("node", key, source); err != nil {
			t.Fatal(err)
		}
	}
	const keyPath = "/etc/apt/keyrings/abr/node.gpg"
	got, err := h.read(keyPath)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("local repository key: %q %v", got, err)
	}
	got, err = h.read("/etc/apt/sources.list.d/abr-node.sources")
	if err != nil || string(got) != source+"Signed-By: "+keyPath+"\n" {
		t.Fatalf("repository source: %q %v", got, err)
	}
	for path, mode := range map[string]os.FileMode{keyPath: 0644, "/etc/apt/keyrings/abr": 0755, "/usr/share/keyrings": 0777} {
		info, err := os.Stat(h.path(path))
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("permissions for %s: %v", path, err)
		}
	}
	entries, err := os.ReadDir(shared)
	if err != nil || len(entries) != 0 {
		t.Fatal("modified unrelated package key directory")
	}
	if err := os.Chmod(h.path("/etc/apt/keyrings/abr"), 0777); err != nil {
		t.Fatal(err)
	}
	if err := h.writeAPTRepository("unsafe", key, source); err == nil {
		t.Fatal("accepted writable local key directory")
	}
	if _, err := h.read("/etc/apt/sources.list.d/abr-unsafe.sources"); !os.IsNotExist(err) {
		t.Fatal("enabled repository with an unsafe key directory")
	}
}

func TestSSHKeyPreflightBeforePackagesAndRejectsWritableKeys(t *testing.T) {
	h, r := setupFixture(t)
	if err := h.removeFile("/home/ubuntu/.ssh/authorized_keys"); err != nil {
		t.Fatal(err)
	}
	if err := h.Setup(SetupOptions{Hostname: "my-vps", RoadRunnerVersion: DefaultRoadRunnerVersion, AdminUser: "ubuntu"}); err == nil {
		t.Fatal("setup without administrator keys succeeded")
	}
	for _, c := range r.calls {
		if c.Name == "apt-get" || c.Name == "ufw" {
			t.Fatal("changed host before verifying administrator keys")
		}
	}
	if err := h.write("/home/ubuntu/.ssh/authorized_keys", []byte(strings.TrimPrefix(githubHostKey, "github.com ")), 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := h.sshPreflight("ubuntu"); err == nil {
		t.Fatal("writable authorized_keys accepted")
	}
	if err := os.Chmod(h.path("/home/ubuntu/.ssh/authorized_keys"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.sshPreflight("ubuntu"); err != nil {
		t.Fatal(err)
	}
}

func TestSSHHardeningValidatesEffectiveSettingsAndRestoresOnFailure(t *testing.T) {
	for _, failure := range []string{"override", "syntax", "reload"} {
		t.Run(failure, func(t *testing.T) {
			h, r := setupFixture(t)
			const path = "/etc/ssh/sshd_config.d/00-abr-hardening.conf"
			old := []byte("# Managed by abr\nPasswordAuthentication yes\n")
			if err := h.write(path, old, 0644); err != nil {
				t.Fatal(err)
			}
			failed := false
			if failure == "override" {
				r.ssh = strings.ReplaceAll(r.ssh, "passwordauthentication no", "passwordauthentication yes")
			} else {
				r.fail = func(c Command) error {
					if !failed && ((failure == "syntax" && c.Name == "/usr/sbin/sshd" && c.Args[0] == "-t") || (failure == "reload" && c.Name == "systemctl")) {
						failed = true
						return testExit(1)
					}
					return nil
				}
			}
			if err := h.hardenSSH(sshAdmin{name: "ubuntu", context: "user=ubuntu,host=localhost,addr=127.0.0.1"}); err == nil {
				t.Fatal("SSH failure reported success")
			}
			after, err := h.read(path)
			if err != nil || !bytes.Equal(after, old) {
				t.Fatal("original SSH authentication file not restored")
			}
		})
	}
}

func TestSSHSocketAndConnectionPortsArePreserved(t *testing.T) {
	h, _ := setupFixture(t)
	t.Setenv("SSH_CONNECTION", "192.0.2.1 40000 192.0.2.2 2022")
	ports, err := h.sshPorts(2222)
	if err != nil || strings.Join(ports, ",") != "2022,2200,2222" {
		t.Fatalf("SSH ports: %v %v", ports, err)
	}
}

func TestMySQLSecurityRefusesUnsafeExistingStateAndSetsLocalPolicy(t *testing.T) {
	for _, bad := range []string{"version", "existing accounts", "hostname accounts"} {
		t.Run(bad, func(t *testing.T) {
			h, r := setupFixture(t)
			if bad == "version" {
				r.version = "9.7.0\n"
			} else if bad == "hostname accounts" {
				r.hostnameAccounts = "1\n"
			} else {
				r.insecure = "1\n"
			}
			if err := h.hardenMySQL(); err == nil {
				t.Fatal("unsupported/unsafe MySQL state accepted")
			}
			for _, c := range r.calls {
				if strings.Contains(string(c.Input), "ALTER USER") || strings.Contains(string(c.Input), "DROP ") {
					t.Fatal("changed unrelated accounts or databases")
				}
			}
		})
	}
	h, r := setupFixture(t)
	if err := h.hardenMySQL(); err != nil {
		t.Fatal(err)
	}
	var sql string
	for _, c := range r.calls {
		sql += string(c.Input)
	}
	if !strings.Contains(sql, "IDENTIFIED WITH auth_socket") || !strings.Contains(sql, "validate_password.policy=2") || !strings.Contains(sql, "validate_password.length=14") || strings.Contains(sql, "DROP ") {
		t.Fatal("missing or destructive MySQL policy")
	}
	if strings.Contains(sql, "Host NOT IN ('localhost', '127.0.0.1')") {
		t.Fatal("unrecorded TCP root was exempt from preflight")
	}
	h2, r2 := setupFixture(t)
	data := fmt.Sprintf(`{"user":"root","host":"127.0.0.1","password":"%sAa1!","ready":false}`, strings.Repeat("a", 64))
	if err := h2.write(h2.mysqlAdminPath(), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h2.hardenMySQL(); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(r2.calls, func(c Command) bool {
		return strings.Contains(string(c.Input), "Host NOT IN ('localhost', '127.0.0.1')")
	}) {
		t.Fatal("recorded partial admin setup cannot be retried")
	}
}

func TestRedisValidationFailureRestoresOriginalConfigs(t *testing.T) {
	h, r := setupFixture(t)
	old := []byte("bind 127.0.0.1\nprotected-mode yes\n")
	if err := h.write("/etc/redis/redis.conf", old, 0640); err != nil {
		t.Fatal(err)
	}
	r.redis = "bind\n0.0.0.0\nprotected-mode\nno\n"
	if err := h.configureRedis(); err == nil {
		t.Fatal("insecure Redis settings accepted")
	}
	after, err := h.read("/etc/redis/redis.conf")
	if err != nil || !bytes.Equal(old, after) {
		t.Fatal("Redis main config not restored")
	}
	if _, err := os.Stat(h.path("/etc/redis/abr.conf")); !os.IsNotExist(err) {
		t.Fatal("failed Redis drop-in remained")
	}
}

func TestRedisSecuresPackagedDirectoryBeforeManagedWrites(t *testing.T) {
	h, r := setupFixture(t)
	if err := h.write("/etc/redis/redis.conf", []byte("bind 127.0.0.1\n"), 0640); err != nil {
		t.Fatal(err)
	}
	dir := h.path("/etc/redis")
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	adopted := false
	r.fail = func(c Command) error {
		if c.Name == "chown" && slices.Equal(c.Args, []string{"--no-dereference", "root:redis", "/etc/redis"}) {
			if _, err := h.read("/etc/redis/abr.conf"); !os.IsNotExist(err) {
				t.Fatal("wrote managed configuration before securing directory")
			}
			adopted = true
		}
		if c.Name == "chmod" && slices.Equal(c.Args, []string{"0750", "/etc/redis"}) {
			if !adopted {
				t.Fatal("changed permissions before adopting the directory")
			}
			// The fake runner cannot alter ownership; this fixture already belongs
			// to the test user. Execute the permission change on its temporary path.
			return os.Chmod(dir, 0750)
		}
		return nil
	}
	if err := h.configureRedis(); err != nil {
		t.Fatal(err)
	}
	if !adopted {
		t.Fatal("did not secure the packaged configuration directory")
	}
	data, err := h.read("/etc/redis/redis.conf")
	if err != nil || !hasDirective(data, "include /etc/redis/abr.conf") {
		t.Fatal("managed Redis configuration was not enabled")
	}
}

func TestRedisRejectsSymlinkBeforeAdoptingDirectory(t *testing.T) {
	h, r := setupFixture(t)
	if err := h.write("/outside/redis.conf", []byte("bind 127.0.0.1\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(h.path("/etc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(h.path("/outside"), h.path("/etc/redis")); err != nil {
		t.Fatal(err)
	}
	r.calls = nil
	if err := h.configureRedis(); err == nil {
		t.Fatal("accepted symlinked Redis configuration directory")
	}
	if len(r.calls) != 0 {
		t.Fatal("changed the host before rejecting a symlinked directory")
	}
}

func TestRedisRejectsSymlinkedConfigWithoutCopyingItsContents(t *testing.T) {
	h, r := setupFixture(t)
	private := []byte("private file outside Redis configuration\n")
	if err := h.write("/private/secret", private, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(h.path("/etc/redis"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(h.path("/private/secret"), h.path("/etc/redis/redis.conf")); err != nil {
		t.Fatal(err)
	}
	r.calls = nil
	if err := h.configureRedis(); err == nil {
		t.Fatal("accepted symlinked Redis configuration")
	}
	for _, c := range r.calls {
		if c.Name != "chown" && c.Name != "chmod" {
			t.Fatal("changed the service before rejecting a symlinked file")
		}
	}
	info, err := os.Lstat(h.path("/etc/redis/redis.conf"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("replaced the symlink with service-readable private contents")
	}
	got, err := h.read("/private/secret")
	if err != nil || !bytes.Equal(got, private) {
		t.Fatal("modified private file")
	}
	if _, err := h.read("/etc/redis/abr.conf"); !os.IsNotExist(err) {
		t.Fatal("installed configuration through an unsafe existing file")
	}
}

func TestSetupRejectsMissingDefaultsAndDownloadCorruption(t *testing.T) {
	if err := requireSetupTemplates(fstest.MapFS{}); err == nil {
		t.Fatal("incomplete defaults accepted")
	}
	templates, err := abr.TemplateFS()
	if err != nil {
		t.Fatal(err)
	}
	if err := requireSetupTemplates(templates); err != nil {
		t.Fatal(err)
	}
	data := []byte("verified release")
	sum := sha256.Sum256(data)
	if err := verifySHA256(data, hex.EncodeToString(sum[:])+"  composer.phar\n"); err != nil {
		t.Fatal(err)
	}
	if verifySHA256(data, "") == nil || verifySHA256(data, strings.Repeat("0", 64)) == nil {
		t.Fatal("invalid checksum accepted")
	}
	if _, err := fetch("http://example.invalid/download", 100); err == nil {
		t.Fatal("unencrypted download accepted")
	}
}

func TestInstallEmbeddedTemplatesPreservesEditsAndFillsMissingFiles(t *testing.T) {
	h, _, _, _ := fixture(t)
	h.TemplatesDir = filepath.Join(h.root, "installed-templates")
	templates, err := abr.TemplateFS()
	if err != nil {
		t.Fatal(err)
	}
	custom := []byte("# locally edited scheduler\n")
	customPath := filepath.Join(h.TemplatesDir, "scheduler.service.tmpl")
	if err := h.write(customPath, custom, 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := h.installTemplates(templates); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := fs.ReadDir(templates, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		path := filepath.Join(h.TemplatesDir, entry.Name())
		want, err := fs.ReadFile(templates, entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0644)
		if path == customPath {
			want, mode = custom, 0600
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("installed %s: %v", entry.Name(), err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("permissions for %s: %v", entry.Name(), err)
		}
	}
}

func TestInstallEmbeddedTemplatesDryRunAndDestinationErrors(t *testing.T) {
	h, _, _, _ := fixture(t)
	h.TemplatesDir = filepath.Join(h.root, "installed-templates")
	templates, err := abr.TemplateFS()
	if err != nil {
		t.Fatal(err)
	}
	h.DryRun = true
	if err := h.installTemplates(templates); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.TemplatesDir); !os.IsNotExist(err) {
		t.Fatal("dry run created template files")
	}
	h.DryRun = false
	if err := os.MkdirAll(filepath.Join(h.TemplatesDir, "automatic-updates.conf.tmpl"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := h.installTemplates(templates); err == nil {
		t.Fatal("invalid destination reported success")
	}
}

func TestCaddyImportRecognizesActiveLinesAndRestoresOnFailure(t *testing.T) {
	const path = "/etc/caddy/Caddyfile"
	const directive = "import /etc/caddy/abr.d/abr-*.caddy"
	for _, existing := range []string{"# " + directive + "\n:80 { respond ok }\n", "  import   /etc/caddy/abr.d/abr-*.caddy # managed\n"} {
		h, r := setupFixture(t)
		if err := h.write(path, []byte(existing), 0644); err != nil {
			t.Fatal(err)
		}
		if err := h.configureCaddyImport(); err != nil {
			t.Fatal(err)
		}
		data, _ := h.read(path)
		if !hasDirective(data, directive) {
			t.Fatal("active Caddy import missing")
		}
		if hasDirective([]byte(existing), directive) && string(data) != existing {
			t.Fatal("duplicated existing import")
		}
		if err := h.write(path, []byte("# existing configuration\n"), 0644); err != nil {
			t.Fatal(err)
		}
		r.fail = func(c Command) error {
			if c.Name == "caddy" {
				return testExit(1)
			}
			return nil
		}
		if err := h.configureCaddyImport(); err == nil {
			t.Fatal("invalid Caddy config reported success")
		}
		data, _ = h.read(path)
		if string(data) != "# existing configuration\n" {
			t.Fatal("Caddy configuration was not restored")
		}
	}
}

func TestCaddyFormattingExistingImportRestoresOnValidationFailure(t *testing.T) {
	h, r := setupFixture(t)
	const path = "/etc/caddy/Caddyfile"
	original := []byte("# Custom configuration\n:80 {\n    respond 404\n}\nimport /etc/caddy/abr.d/abr-*.caddy\n")
	if err := h.write(path, original, 0644); err != nil {
		t.Fatal(err)
	}
	formatted := false
	r.fail = func(c Command) error {
		if c.Name != "caddy" {
			return nil
		}
		if c.Args[0] == "fmt" {
			formatted = true
			return os.WriteFile(h.path(path), bytes.ReplaceAll(original, []byte("    "), []byte("\t")), 0644)
		}
		if c.Args[0] == "validate" {
			if !formatted {
				t.Fatal("validated before formatting")
			}
			return testExit(1)
		}
		return nil
	}
	if err := h.configureCaddyImport(); err == nil {
		t.Fatal("invalid formatted configuration accepted")
	}
	data, err := h.read(path)
	if err != nil || !bytes.Equal(data, original) {
		t.Fatal("failed validation did not restore custom configuration")
	}
}

func TestRedisCommentedIncludeDoesNotSkipHardening(t *testing.T) {
	h, _ := setupFixture(t)
	if err := h.write("/etc/redis/redis.conf", []byte("# include /etc/redis/abr.conf\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := h.configureRedis(); err != nil {
		t.Fatal(err)
	}
	data, _ := h.read("/etc/redis/redis.conf")
	if !hasDirective(data, "include /etc/redis/abr.conf") {
		t.Fatal("comment prevented Redis hardening")
	}
}

func TestCaddyAdminUsesPrivateSocketAndRejectsFailedConfiguration(t *testing.T) {
	h, r := setupFixture(t)
	if err := h.configureCaddyAdmin(); err != nil {
		t.Fatal(err)
	}
	const path = "/etc/systemd/system/caddy.service.d/abr-admin.conf"
	data, err := h.read(path)
	if err != nil || !bytes.Contains(data, []byte("unix//var/lib/caddy/abr-admin.sock")) {
		t.Fatal("private Caddy administration socket missing")
	}
	r.fail = func(c Command) error {
		if c.Name == "systemctl" {
			return testExit(1)
		}
		return nil
	}
	if err := h.configureCaddyAdmin(); err == nil {
		t.Fatal("failed Caddy restart reported success")
	}
	after, _ := h.read(path)
	if !bytes.Equal(data, after) {
		t.Fatal("Caddy service config not restored")
	}
}

func TestRedisConvertsLiveDatasetBeforeConfigAndRestart(t *testing.T) {
	h, r := setupFixture(t)
	if err := h.write("/etc/redis/redis.conf", []byte("bind 127.0.0.1\n"), 0640); err != nil {
		t.Fatal(err)
	}
	r.fail = func(c Command) error {
		if c.Name == "redis-cli" && slices.Contains(c.Args, "SET") {
			if _, err := os.Stat(h.path("/etc/redis/abr.conf")); !os.IsNotExist(err) {
				t.Fatal("changed config before live conversion")
			}
		}
		return nil
	}
	if err := h.configureRedis(); err != nil {
		t.Fatal(err)
	}
	converted, persisted := false, false
	for _, c := range r.calls {
		if c.Name == "redis-cli" && slices.Contains(c.Args, "SET") {
			converted = true
		}
		if c.Name == "redis-cli" && slices.Contains(c.Args, "INFO") {
			persisted = converted
		}
		if c.Name == "systemctl" && slices.Contains(c.Args, "restart") && !persisted {
			t.Fatal("restarted before durable AOF")
		}
	}
	if !persisted {
		t.Fatal("did not verify AOF persistence")
	}
	r.calls = nil
	r.fail = func(c Command) error {
		if c.Name == "redis-cli" && slices.Contains(c.Args, "INFO") {
			return testExit(1)
		}
		return nil
	}
	if err := h.configureRedis(); err == nil {
		t.Fatal("failed conversion reported success")
	}
	for _, c := range r.calls {
		if c.Name == "systemctl" && slices.Contains(c.Args, "restart") {
			t.Fatal("restarted despite failed AOF verification")
		}
	}
}

func TestCaddyReplacesOnlyPackagedWelcomeAndRestoresInvalidReplacement(t *testing.T) {
	const path = "/etc/caddy/Caddyfile"
	h, r := setupFixture(t)
	stock := []byte("# Packaged welcome\n:80 {\n root * /usr/share/caddy\n file_server\n}\n")
	if err := h.write(path, stock, 0644); err != nil {
		t.Fatal(err)
	}
	if err := h.configureCaddyImport(); err != nil {
		t.Fatal(err)
	}
	data, _ := h.read(path)
	if strings.Contains(string(data), "/usr/share/caddy") || !hasDirective(data, "-Server") || !hasDirective(data, "-Via") || !strings.Contains(string(data), "respond 404") {
		t.Fatal(string(data))
	}
	if err := h.configureCaddyImport(); err != nil {
		t.Fatal(err)
	}
	repeated, _ := h.read(path)
	if !bytes.Equal(data, repeated) {
		t.Fatal("default configuration not idempotent")
	}
	if err := h.write(path, stock, 0644); err != nil {
		t.Fatal(err)
	}
	r.fail = func(c Command) error {
		if c.Name == "caddy" {
			return testExit(1)
		}
		return nil
	}
	if err := h.configureCaddyImport(); err == nil {
		t.Fatal("invalid replacement accepted")
	}
	restored, _ := h.read(path)
	if !bytes.Equal(restored, stock) {
		t.Fatal("original welcome config not restored")
	}
}
