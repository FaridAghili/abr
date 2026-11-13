package host

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"abr/internal/config"
	"abr/internal/ports"
	"abr/internal/project"
)

// PrepareEnv skips projects without an example. Otherwise it copies the example
// only when .env is absent, then applies recorded database credentials and a
// Laravel app's own Redis databases.
func (h Host) PrepareEnv(name string) error {
	return h.locked(func() error {
		a, r, err := h.application(name)
		if err != nil {
			return err
		}
		if err := h.project(a); err != nil {
			return err
		}
		if h.DryRun {
			h.say("Would copy .env.example to .env if absent for %s when the example exists, preserve existing settings and fill managed MySQL values when enabled and a Laravel app's own Redis databases; skip preparation if no example exists", name)
			return nil
		}
		exists, err := project.HasEnvExample(h.path(a.Directory))
		if err != nil {
			return err
		}
		if !exists {
			h.say("Skipped .env preparation for %s: no .env.example; existing files left untouched", name)
			return nil
		}
		if _, err := h.environmentAccount(a); err != nil {
			return err
		}
		path := h.path(filepath.Join(a.Directory, ".env"))
		data, err := readProjectEnv(path)
		fresh := os.IsNotExist(err)
		if fresh {
			data, err = readProjectEnv(h.path(filepath.Join(a.Directory, ".env.example")))
		}
		if err != nil {
			return fmt.Errorf("read .env or .env.example: %w", err)
		}
		if fresh && a.Type == "laravel" {
			data = setEnvValues(data, [][2]string{{"APP_ENV", "production"}, {"APP_DEBUG", "false"}, {"APP_URL", "https://" + a.Domain}})
		}
		if a.Database.Enabled {
			c, err := h.transferCredentials(a)
			if err != nil {
				return err
			}
			if !c.TCPReady {
				return fmt.Errorf("managed MySQL loopback account is not ready")
			}
			data = setEnvValues(data, [][2]string{{"DB_CONNECTION", "mysql"}, {"DB_HOST", "127.0.0.1"}, {"DB_PORT", "3306"}, {"DB_DATABASE", c.Database}, {"DB_USERNAME", c.User}, {"DB_PASSWORD", c.Password}})
		}
		var redis [][2]string
		if a.Type == "laravel" {
			if redis, err = h.redisValues(a, r); err != nil {
				return err
			}
			data = setEnvValues(data, redis)
		}
		if err := h.writeEnvironment(a, data); err != nil {
			return err
		}
		if fresh {
			h.say("Copied .env.example to %s", filepath.Join(a.Directory, ".env"))
		} else {
			h.say("Preserved existing %s", filepath.Join(a.Directory, ".env"))
		}
		if a.Database.Enabled {
			h.say("Set managed MySQL values in .env; password remains private")
		}
		if redis != nil {
			h.say("Set this app's own Redis databases in .env: %s", formatEnvValues(redisPublicValues(redis)))
		}
		h.say("Review .env for app secrets before deployment")
		return nil
	})
}

// redisValues points a Laravel app at its own pair of Redis databases, so
// cache:clear flushes only its cache, and prefixes its keys and broadcast
// channels, which Redis shares across databases. An app without a reserved
// pair, such as one registered before pairs existed, gets one here.
func (h Host) redisValues(a config.App, r ports.Registry) ([][2]string, error) {
	database, ok := r.RedisDatabase(a.Name)
	if !ok {
		var err error
		if r, err = h.Manager.Allocate(); err != nil {
			return nil, err
		}
		if database, ok = r.RedisDatabase(a.Name); !ok {
			return nil, fmt.Errorf("%s has no Redis databases reserved", a.Name)
		}
	}
	values := [][2]string{{"REDIS_DB", strconv.Itoa(database)}, {"REDIS_CACHE_DB", strconv.Itoa(database + 1)}, {"REDIS_PREFIX", a.Name + "-"}}
	access, err := h.redisAccessValues(a)
	if err != nil {
		return nil, err
	}
	return append(values, access...), nil
}

// useOwnRedis writes the app's Redis values into .env only when they differ.
func (h Host) useOwnRedis(a config.App, r ports.Registry) error {
	if h.DryRun {
		h.say("Would set REDIS_DB, REDIS_CACHE_DB and REDIS_PREFIX in .env to this app's own Redis databases")
		return nil
	}
	values, err := h.redisValues(a, r)
	if err != nil {
		return err
	}
	if err := h.checkRedisDatabases(a, values); err != nil {
		return err
	}
	data, err := readProjectEnv(h.path(filepath.Join(a.Directory, ".env")))
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(values, func(pair [2]string) bool { return dotenvValue(data, pair[0]) != pair[1] }) {
		return nil
	}
	if err := h.writeEnvironment(a, setEnvValues(data, values)); err != nil {
		return err
	}
	h.note("Set this app's own Redis databases in .env: %s", formatEnvValues(redisPublicValues(values)))
	return nil
}

// checkRedisDatabases makes sure the running Redis has the app's databases;
// a server set up before they were reserved needs setup to apply them.
func (h Host) checkRedisDatabases(a config.App, values [][2]string) error {
	if managed, err := h.redisManaged(); err != nil || !managed {
		return err // Set up without Redis.
	}
	out, err := h.redis("Check Redis databases", "CONFIG", "GET", "databases")
	if err != nil {
		return err
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 || fields[0] != "databases" {
		return fmt.Errorf("cannot read the number of Redis databases")
	}
	available, err := strconv.Atoi(fields[1])
	if err != nil {
		return fmt.Errorf("cannot read the number of Redis databases: %w", err)
	}
	cache, _ := strconv.Atoi(values[1][1])
	if cache >= available {
		return fmt.Errorf("%s uses Redis databases %s and %d, but Redis has %d; run abr setup to apply the managed Redis configuration, then deploy again", a.Name, values[0][1], cache, available)
	}
	return nil
}

// redisPublicValues omits the password from messages.
func redisPublicValues(values [][2]string) [][2]string {
	var public [][2]string
	for _, pair := range values {
		if pair[0] != "REDIS_PASSWORD" {
			public = append(public, pair)
		}
	}
	return public
}

func formatEnvValues(values [][2]string) string {
	pairs := make([]string, len(values))
	for i, pair := range values {
		pairs[i] = pair[0] + "=" + pair[1]
	}
	return strings.Join(pairs, " ")
}

func setEnvValues(data []byte, values [][2]string) []byte {
	lines := strings.Split(strings.TrimSuffix(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n"), "\n")
	for _, pair := range values {
		var next []string
		found := false
		for _, line := range lines {
			key, _, assignment := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "export "), "=")
			if assignment && strings.TrimSpace(key) == pair[0] {
				if !found {
					next = append(next, pair[0]+"="+pair[1])
				}
				found = true
			} else {
				next = append(next, line)
			}
		}
		if !found {
			next = append(next, pair[0]+"="+pair[1])
		}
		lines = next
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// Write as the recorded app user, using a private temporary file and an atomic
// rename. Root never writes through a path controlled by project code.
func (h Host) writeEnvironment(a config.App, data []byte) (result error) {
	// Unlike streamed deploy commands, mktemp must return its filename.
	creator := environmentProcess(a, "mktemp", "--", ".abr-env-XXXXXXXXXX")
	output, err := h.run("Create private environment file as "+a.User, Command{Name: "runuser", Args: creator.Args[1:], Dir: h.path(a.Directory), Private: true})
	if err != nil {
		return err
	}
	temp := strings.TrimSpace(string(output))
	if filepath.Base(temp) != temp || !strings.HasPrefix(temp, ".abr-env-") || strings.ContainsAny(temp, "\x00\r\n") {
		return fmt.Errorf("invalid temporary environment filename")
	}
	defer func() {
		_, err := h.asUser(a, nil, true, "rm", "-f", "--", temp)
		result = errors.Join(result, err)
	}()
	// Use a fixed environment and command without shell parsing.
	writer := environmentProcess(a, "tee", "--", temp)
	_, err = h.run("Write private app environment", Command{Name: "runuser", Args: writer.Args[1:], Dir: h.path(a.Directory), Input: data, Private: true})
	if err != nil {
		return err
	}
	if _, err := h.asUser(a, nil, true, "chmod", "600", "--", temp); err != nil {
		return err
	}
	_, err = h.asUser(a, nil, true, "mv", "-fT", "--", temp, ".env")
	return err
}

func (h Host) environmentAccount(a config.App) (userRecord, error) {
	var record userRecord
	data, err := h.read(h.userPath(a))
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, err
	}
	entry, exists, err := h.passwd(a.User)
	if err != nil {
		return record, err
	}
	parts := strings.Split(entry, ":")
	if !exists || !validRuntimeAccount(parts, a.User) || record.App != a.Name || record.User != a.User || record.UID != parts[2] || record.Home != "/var/lib/abr-users/"+a.User || parts[5] != record.Home || parts[4] != "abr-"+a.Name {
		return record, fmt.Errorf("app environment requires its recorded dedicated Ubuntu account")
	}
	return record, nil
}

func environmentProcess(a config.App, program string, args ...string) *exec.Cmd {
	return appProcess(a, nil, program, args...)
}

func appProcess(a config.App, environment map[string]string, program string, args ...string) *exec.Cmd {
	command := []string{"--user", a.User, "--", "env", "-i", "HOME=/var/lib/abr-users/" + a.User, "USER=" + a.User, "LOGNAME=" + a.User, "LANG=C.UTF-8", "PATH=/usr/local/bin:/usr/bin:/bin"}
	for _, key := range slices.Sorted(maps.Keys(environment)) {
		if key != "PATH" && key != "HOME" && key != "USER" && key != "LOGNAME" {
			command = append(command, key+"="+environment[key])
		}
	}
	command = append(command, "TERM="+os.Getenv("TERM"))
	command = append(command, "/usr/bin/setpriv", "--no-new-privs", "--", program)
	command = append(command, args...)
	cmd := exec.Command("/usr/sbin/runuser", command...)
	cmd.Dir = a.Directory
	cmd.Env = []string{"PATH=" + hostPath, "LANG=C.UTF-8"}
	return cmd
}

// EnvEditor returns a foreground process for the TUI to run while its renderer
// is suspended, and a check for content changes after it exits. An existing .env
// is required, but .env.example is not. The editor runs as the app user.
func (h Host) EnvEditor(name string) (*exec.Cmd, func() (bool, error), error) {
	var command *exec.Cmd
	var changed func() (bool, error)
	err := h.locked(func() error {
		if h.DryRun {
			return fmt.Errorf("environment editing is unavailable in preview mode")
		}
		a, _, err := h.application(name)
		if err != nil {
			return err
		}
		if err := h.project(a); err != nil {
			return err
		}
		if _, err := h.environmentAccount(a); err != nil {
			return err
		}
		path := h.path(filepath.Join(a.Directory, ".env"))
		data, err := readProjectEnv(path)
		if err != nil {
			return err
		}
		before := sha256.Sum256(data)
		changed = func() (bool, error) {
			var different bool
			err := h.locked(func() error {
				if err := h.project(a); err != nil {
					return err
				}
				data, err := readProjectEnv(path)
				if err != nil {
					return err
				}
				different = sha256.Sum256(data) != before
				return nil
			})
			return different, err
		}
		command = environmentProcess(a, "/usr/bin/nano", "--", ".env")
		command.Dir = h.path(a.Directory)
		return nil
	})
	return command, changed, err
}
