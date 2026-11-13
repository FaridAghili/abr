package host

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"abr/internal/config"
	"abr/internal/ports"
)

func TestRedisAccountsIsolateAppsAndLockTheDefaultUser(t *testing.T) {
	h, r := setupFixture(t)
	a := fixtureApp(h)
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.write("/etc/redis/redis.conf", []byte("bind 127.0.0.1\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := h.configureRedis(); err != nil {
		t.Fatal(err)
	}
	admin, err := h.readRedisAdmin()
	if err != nil {
		t.Fatal(err)
	}
	acl, err := h.read(redisACLPath)
	if err != nil {
		t.Fatal(err)
	}
	access, err := h.readRedisAccess(a.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"user default on " + redisHash(admin.Password) + " ~* &* +@all\n", "user abr-app on " + redisHash(access.Password) + " ~app-* &* +@all -@dangerous +select +flushdb +info +client|setinfo +client|setname +client|getname\n"} {
		if !strings.Contains(string(acl), want) {
			t.Fatalf("ACL file lacks %q:\n%s", want, acl)
		}
	}
	if strings.Contains(string(acl), admin.Password) || strings.Contains(string(acl), access.Password) {
		t.Fatal("ACL file holds plaintext passwords")
	}
	conf, _ := h.read("/etc/redis/abr.conf")
	if !hasDirective(conf, "aclfile "+redisACLPath) {
		t.Fatal("Redis does not load the ACL file")
	}
	// The running app got its credentials, refreshed its cached config and restarted.
	env, _ := os.ReadFile(filepath.Join(a.Directory, ".env"))
	if dotenvValue(env, "REDIS_USERNAME") != "abr-app" || dotenvValue(env, "REDIS_PASSWORD") != access.Password {
		t.Fatalf("app .env lacks its Redis account:\n%s", env)
	}
	authenticated := false
	for _, c := range r.calls {
		if c.Name == "redis-cli" {
			if !slices.Contains(c.Env, "REDISCLI_AUTH="+admin.Password) || slices.ContainsFunc(c.Args, func(arg string) bool { return strings.Contains(arg, admin.Password) }) {
				t.Fatalf("redis-cli without private authentication: %v", c.Args)
			}
			authenticated = true
		}
		if strings.Contains(strings.Join(c.Args, " "), access.Password) || strings.Contains(strings.Join(c.Args, " "), admin.Password) {
			t.Fatal("Redis password in command arguments")
		}
	}
	if !authenticated {
		t.Fatal("Redis never checked")
	}
	if out := h.Output.(interface{ String() string }).String(); strings.Contains(out, access.Password) || strings.Contains(out, admin.Password) {
		t.Fatal("Redis password printed")
	}
	// Purging the app deletes its account; the administrator stays.
	if err := h.Purge(a.Name, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(h.path(h.redisAccessPath(a.Name))); !os.IsNotExist(err) {
		t.Fatal("purged app kept its Redis account")
	}
	acl, _ = h.read(redisACLPath)
	if strings.Contains(string(acl), "abr-app") || !strings.Contains(string(acl), "user default on") {
		t.Fatalf("ACL not updated after purge:\n%s", acl)
	}
}

func TestRedisAccountIsCreatedOnFirstEnvAndKeptAcrossDeployments(t *testing.T) {
	h, r, _, a := fixture(t)
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	// Without the administrator record, apps keep connecting without credentials.
	if err := h.write("/etc/redis/abr.conf", []byte("# Managed by abr\n"), 0640); err != nil {
		t.Fatal(err)
	}
	values, err := h.redisValues(a, mustRegistry(t, h))
	if err != nil || len(values) != 3 {
		t.Fatalf("credentials before setup locked Redis: %v %v", values, err)
	}
	if _, err := h.ensureRedisAdmin(); err != nil {
		t.Fatal(err)
	}
	values, err = h.redisValues(a, mustRegistry(t, h))
	if err != nil || len(values) != 5 || values[3][0] != "REDIS_USERNAME" || values[3][1] != "abr-app" || values[4][0] != "REDIS_PASSWORD" {
		t.Fatalf("app Redis account missing: %v %v", values, err)
	}
	loaded := 0
	for _, c := range r.calls {
		if c.Name == "redis-cli" && slices.Equal(c.Args, []string{"ACL", "LOAD"}) {
			loaded++
		}
	}
	if loaded != 1 {
		t.Fatalf("ACL loaded %d times, want once", loaded)
	}
	again, err := h.redisValues(a, mustRegistry(t, h))
	if err != nil || !slices.Equal(again[4][:], values[4][:]) {
		t.Fatal("password changed between calls")
	}
}

func fixtureApp(h Host) config.App {
	return config.App{Name: "app", User: "abr-app", Directory: filepath.Join(h.AppsDir, "app"), Type: "laravel", Domain: "app.localhost", Web: config.Web{Driver: "fpm"}}
}

func mustRegistry(t *testing.T, h Host) ports.Registry {
	t.Helper()
	r, err := ports.Load(h.Manager.RegistryPath())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRedisAdminShowsPasswordOnlyOnRequest(t *testing.T) {
	h, _, out, _ := fixture(t)
	if err := h.RedisAdmin(true); err == nil || !strings.Contains(err.Error(), "not managed") {
		t.Fatal("unmanaged Redis reported a password", err)
	}
	if err := h.write("/etc/redis/abr.conf", []byte("# Managed by abr\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := h.RedisAdmin(true); err == nil || !strings.Contains(err.Error(), "run abr setup") {
		t.Fatal("missing record not explained", err)
	}
	admin, err := h.ensureRedisAdmin()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RedisAdmin(false); err != nil || strings.Contains(out.String(), admin.Password) || !strings.Contains(out.String(), "6379") {
		t.Fatalf("password shown without --show: %v\n%s", err, out.String())
	}
	if err := h.RedisAdmin(true); err != nil || !strings.Contains(out.String(), admin.Password) {
		t.Fatalf("password not shown with --show: %v", err)
	}
}
