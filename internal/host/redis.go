package host

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"abr/internal/config"
	"abr/internal/ports"
	"abr/internal/services"
	"abr/internal/table"
)

// Redis is shared by every app, so each Laravel app authenticates as its own
// Redis user that may only touch keys with its own prefix. The default user
// gets the server password, which only root reads; unauthenticated local
// connections then have no access.
const redisACLPath = "/etc/redis/abr.acl"

type redisAdmin struct {
	Password string `json:"password"`
}

// redisAccess is one app's Redis account; its key pattern follows REDIS_PREFIX.
type redisAccess struct {
	App      string `json:"app"`
	User     string `json:"user"`
	Password string `json:"password"`
}

var redisPasswordPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (h Host) redisAdminPath() string { return filepath.Join(h.Manager.StateDir, "redis-admin.json") }
func (h Host) redisAccessPath(n string) string {
	return filepath.Join(h.Manager.StateDir, "redis", n+".json")
}
func redisUser(name string) string { return "abr-" + name }

func redisSecret() (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	return hex.EncodeToString(secret), nil
}

// redisManaged reports whether setup installed abr's Redis configuration.
func (h Host) redisManaged() (bool, error) {
	_, err := os.Lstat(h.path("/etc/redis/abr.conf"))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func (h Host) readRedisAdmin() (redisAdmin, error) {
	var a redisAdmin
	data, err := h.read(h.redisAdminPath())
	if err != nil {
		return a, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&a) != nil || decoder.Decode(new(any)) != io.EOF || !redisPasswordPattern.MatchString(a.Password) {
		return a, fmt.Errorf("invalid Redis admin record")
	}
	return a, nil
}

// ensureRedisAdmin records the server password before it is applied, so a
// retried setup applies the same one.
func (h Host) ensureRedisAdmin() (redisAdmin, error) {
	a, err := h.readRedisAdmin()
	if err == nil {
		return a, nil
	}
	if !os.IsNotExist(err) {
		return a, err
	}
	a.Password, err = redisSecret()
	if err != nil {
		return a, err
	}
	data, _ := json.MarshalIndent(a, "", "  ")
	return a, h.write(h.redisAdminPath(), append(data, '\n'), 0600)
}

// redisAccessManaged reports whether apps authenticate with their own users.
func (h Host) redisAccessManaged() (bool, error) {
	managed, err := h.redisManaged()
	if err != nil || !managed {
		return false, err
	}
	_, err = h.readRedisAdmin()
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

// redis runs redis-cli as the server administrator. The password travels in the
// environment redis-cli reads, never in arguments; without a record the server
// still accepts unauthenticated local clients.
func (h Host) redis(label string, args ...string) ([]byte, error) {
	c := Command{Name: "redis-cli", Args: args, Private: true}
	admin, err := h.readRedisAdmin()
	if err == nil {
		c.Env = []string{"REDISCLI_AUTH=" + admin.Password}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return h.run(label, c)
}

func (h Host) readRedisAccess(name string) (redisAccess, error) {
	var a redisAccess
	data, err := h.read(h.redisAccessPath(name))
	if err != nil {
		return a, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&a) != nil || decoder.Decode(new(any)) != io.EOF || a.App != name || a.User != redisUser(name) || !redisPasswordPattern.MatchString(a.Password) {
		return a, fmt.Errorf("invalid Redis account record for %s", name)
	}
	return a, nil
}

// ensureRedisAccess returns the app's Redis account, creating and loading it
// into the running server when it is new.
func (h Host) ensureRedisAccess(a config.App) (redisAccess, error) {
	access, err := h.readRedisAccess(a.Name)
	if err == nil {
		return access, nil
	}
	if !os.IsNotExist(err) {
		return access, err
	}
	access = redisAccess{App: a.Name, User: redisUser(a.Name)}
	if access.Password, err = redisSecret(); err != nil {
		return access, err
	}
	data, _ := json.MarshalIndent(access, "", "  ")
	if err := h.write(h.redisAccessPath(a.Name), append(data, '\n'), 0600); err != nil {
		return access, err
	}
	return access, h.applyRedisACL()
}

// forgetRedisAccess deletes a purged app's Redis account.
func (h Host) forgetRedisAccess(name string) error {
	if _, err := os.Lstat(h.path(h.redisAccessPath(name))); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := h.removeFile(h.redisAccessPath(name)); err != nil {
		return err
	}
	managed, err := h.redisAccessManaged()
	if err != nil || !managed {
		return err
	}
	return h.applyRedisACL()
}

// redisAccounts lists every recorded app account, by app name.
func (h Host) redisAccounts() ([]redisAccess, error) {
	entries, err := os.ReadDir(h.path(filepath.Join(h.Manager.StateDir, "redis")))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var accounts []redisAccess
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || !entry.Type().IsRegular() || !config.ValidName(name) {
			continue
		}
		access, err := h.readRedisAccess(name)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, access)
	}
	slices.SortFunc(accounts, func(a, b redisAccess) int { return strings.Compare(a.App, b.App) })
	return accounts, nil
}

func redisHash(password string) string {
	sum := sha256.Sum256([]byte(password))
	return "#" + hex.EncodeToString(sum[:])
}

// redisACL renders the ACL file: the administrator keeps everything; each app
// may use only keys with its prefix. Of the dangerous group, apps keep FLUSHDB
// for Laravel's cache:clear (each app flushes only its own databases), INFO for
// Horizon's dashboard, and the CLIENT subcommands phpredis sends when connecting.
func redisACL(admin redisAdmin, accounts []redisAccess) []byte {
	var b strings.Builder
	b.WriteString("# Managed by abr; each Laravel app authenticates as its own user.\n")
	fmt.Fprintf(&b, "user default on %s ~* &* +@all\n", redisHash(admin.Password))
	for _, a := range accounts {
		fmt.Fprintf(&b, "user %s on %s ~%s-* &* +@all -@dangerous +select +flushdb +info +client|setinfo +client|setname +client|getname\n", a.User, redisHash(a.Password), a.App)
	}
	return []byte(b.String())
}

// writeRedisACL installs the ACL file Redis loads at startup.
func (h Host) writeRedisACL() error {
	admin, err := h.readRedisAdmin()
	if err != nil {
		return err
	}
	accounts, err := h.redisAccounts()
	if err != nil {
		return err
	}
	if err := h.write(redisACLPath, redisACL(admin, accounts), 0640); err != nil {
		return err
	}
	// Atomic replacement creates a root-owned file; Redis reads it as its group.
	return h.command("chown", "root:redis", redisACLPath)
}

// applyRedisACL writes the ACL file and makes the running server use it.
func (h Host) applyRedisACL() error {
	if h.DryRun {
		h.say("Would update the Redis users and load them into the running server")
		return nil
	}
	if err := h.writeRedisACL(); err != nil {
		return err
	}
	out, err := h.redis("Load Redis users", "ACL", "LOAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) != "OK" {
		return fmt.Errorf("Redis did not load its users")
	}
	return nil
}

// redisAccessValues are the .env values that make a Laravel app authenticate
// as its own Redis user. Without managed access they are empty.
func (h Host) redisAccessValues(a config.App) ([][2]string, error) {
	managed, err := h.redisAccessManaged()
	if err != nil || !managed {
		return nil, err
	}
	access, err := h.ensureRedisAccess(a)
	if err != nil {
		return nil, err
	}
	return [][2]string{{"REDIS_USERNAME", access.User}, {"REDIS_PASSWORD", access.Password}}, nil
}

// applyRedisAccess gives every registered Laravel app its Redis account after
// setup has locked the server. Running apps get the new values in their
// cached configuration and restart, so none keeps connecting without them.
func (h Host) applyRedisAccess() error {
	c, err := config.Load(h.Manager.ConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	r, err := ports.Load(h.Manager.RegistryPath())
	if err != nil {
		return err
	}
	var failures []error
	for _, a := range c.Apps {
		if a.Type != "laravel" {
			continue
		}
		if _, err := os.Lstat(h.path(filepath.Join(a.Directory, ".env"))); err != nil {
			continue // Not deployed yet; abr env and deploy write the values.
		}
		if err := h.useOwnRedis(a, r); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", a.Name, err))
			continue
		}
		m, exists, err := h.loadManifest(a)
		if err != nil || !exists || !m.Enabled {
			if err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", a.Name, err))
			}
			continue
		}
		if _, err := os.Stat(h.path(filepath.Join(a.Directory, "vendor/autoload.php"))); err != nil {
			continue
		}
		php := "/usr/bin/php" + services.PHPVersion
		if _, err := h.asUser(a, nil, true, php, "artisan", "config:cache", "--no-interaction"); err != nil {
			failures = append(failures, fmt.Errorf("%s: refresh cached configuration: %w", a.Name, err))
			continue
		}
		if len(m.Units) > 0 {
			if err := h.command("systemctl", append([]string{"restart"}, m.Units...)...); err != nil {
				failures = append(failures, fmt.Errorf("%s: restart services: %w", a.Name, err))
			}
		}
		h.say("%s now authenticates to Redis as %s", a.Name, redisUser(a.Name))
	}
	return errors.Join(failures...)
}

// RedisAdmin shows how to connect a desktop client over SSH to Redis. The
// server password is shown only on request; without the record Redis still
// accepts unauthenticated local clients.
func (h Host) RedisAdmin(show bool) error {
	return h.locked(func() error {
		if h.DryRun {
			h.say("Would read Redis connection details; no password displayed")
			return nil
		}
		managed, err := h.redisManaged()
		if err != nil {
			return err
		}
		if !managed {
			return fmt.Errorf("Redis is not managed on this server; run abr setup without --no-redis")
		}
		admin, err := h.readRedisAdmin()
		if os.IsNotExist(err) {
			return fmt.Errorf("Redis authentication is not configured yet; run abr setup to lock Redis and create the password")
		}
		if err != nil {
			return err
		}
		login := table.Table{
			Title:   "TablePlus · Redis via SSH",
			Headers: []string{"FIELD", "VALUE"},
			Rows: [][]table.Cell{
				{table.Styled("Host", table.Strong), table.Text("127.0.0.1")},
				{table.Styled("Port", table.Strong), table.Text("6379")},
				{table.Styled("User", table.Strong), table.Styled("leave blank, or default", table.Muted)},
				{table.Styled("Database", table.Strong), table.Styled("0 to browse; each app uses its own pair from 10 upwards", table.Muted)},
			},
		}
		if show {
			login.Rows = append(login.Rows, []table.Cell{table.Styled("Password", table.Strong), table.Styled(admin.Password, table.Warn)})
		}
		login.Rows = append(login.Rows, []table.Cell{table.Styled("SSH", table.Strong), table.Text("your VPS address, SSH port, administrator user and SSH key")})
		if err := h.writeTable(login); err != nil {
			return err
		}
		if !show {
			h.say("Server password saved privately in %s; view with abr database --redis-admin --show", h.redisAdminPath())
		}
		return nil
	})
}
