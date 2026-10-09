package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"abr/internal/config"
	"abr/internal/project"
)

// PrepareEnv skips projects without an example. Otherwise it copies the example
// only when .env is absent, then applies recorded database credentials.
func (h Host) PrepareEnv(name string) error {
	return h.locked(func() error {
		a, _, err := h.application(name)
		if err != nil {
			return err
		}
		if err := h.project(a); err != nil {
			return err
		}
		if h.DryRun {
			h.say("Would copy .env.example to .env if absent for %s when the example exists, preserve existing settings and fill managed MySQL values when enabled; skip preparation if no example exists", name)
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
		h.say("Review .env for app secrets before deployment")
		return nil
	})
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
// is suspended. No example means no editor (nil, nil). The editor runs with the
// app's identity, never as root.
func (h Host) EnvEditor(name string) (*exec.Cmd, error) {
	var command *exec.Cmd
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
		exists, err := project.HasEnvExample(h.path(a.Directory))
		if err != nil || !exists {
			return err
		}
		if _, err := h.environmentAccount(a); err != nil {
			return err
		}
		if _, err := readProjectEnv(h.path(filepath.Join(a.Directory, ".env"))); err != nil {
			return err
		}
		command = environmentProcess(a, "/usr/bin/nano", "--", ".env")
		command.Dir = h.path(a.Directory)
		return nil
	})
	return command, err
}
