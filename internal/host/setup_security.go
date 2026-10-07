package host

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"abr/internal/config"
)

type sshAdmin struct {
	name, context string
}

func sshValues(data []byte) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, " ")
		if ok {
			values[key] = strings.TrimSpace(value)
		}
	}
	return values
}

// Check for a usable key before installing packages or changing authentication.
// The owner still verifies a real second SSH session before setup.
func (h Host) sshPreflight(name string) (sshAdmin, error) {
	if name == "" {
		name = os.Getenv("SUDO_USER")
		if name == "" {
			name = "root"
		}
	}
	// Reuse the portable Unix username validation, permitting root as an admin.
	a := config.App{Name: "admin", User: name, Directory: "/srv/admin", Domain: "admin.test", Type: "nuxt"}
	if name != "root" && a.Validate() != nil {
		return sshAdmin{}, fmt.Errorf("invalid SSH administrator username")
	}
	admin := sshAdmin{name: name, context: "user=" + name + ",host=localhost,addr=127.0.0.1"}
	if connection := strings.Fields(os.Getenv("SSH_CONNECTION")); len(connection) == 4 && net.ParseIP(connection[0]) != nil {
		admin.context = "user=" + name + ",host=" + connection[0] + ",addr=" + connection[0]
	}
	if h.DryRun {
		h.say("Would verify SSH public-key access for %s before changes", name)
		return admin, nil
	}
	entry, exists, err := h.passwd(name)
	if err != nil {
		return admin, err
	}
	parts := strings.Split(entry, ":")
	if !exists || len(parts) != 7 || !filepath.IsAbs(parts[5]) {
		return admin, fmt.Errorf("SSH administrator %s does not have a valid account/home", name)
	}
	uid, err := strconv.Atoi(parts[2])
	if err != nil {
		return admin, err
	}
	out, err := h.run("Read effective SSH settings for "+name, Command{Name: "/usr/sbin/sshd", Args: []string{"-T", "-C", admin.context}, Private: true})
	if err != nil {
		return admin, err
	}
	settings := sshValues(out)
	for _, key := range strings.Fields(settings["authorizedkeysfile"]) {
		key = strings.NewReplacer("%h", parts[5], "%u", name, "%U", parts[2]).Replace(key)
		if !filepath.IsAbs(key) {
			key = filepath.Join(parts[5], key)
		}
		info, err := os.Stat(h.path(key))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			continue
		}
		secure := true
		for path := key; ; path = filepath.Dir(path) {
			info, err := os.Stat(h.path(path))
			if err != nil || info.Mode().Perm()&0022 != 0 {
				secure = false
				break
			}
			if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != 0 && int(stat.Uid) != uid {
				secure = false
				break
			}
			if path == parts[5] || path == filepath.Dir(path) {
				break
			}
		}
		if secure {
			if _, err := h.run("Verify administrator authorized SSH keys", Command{Name: "ssh-keygen", Args: []string{"-l", "-f", h.path(key)}, Private: true}); err == nil {
				return admin, nil
			}
		}
	}
	return admin, fmt.Errorf("no usable authorized SSH key for %s; install and test its key login first (or select --admin-user)", name)
}

// A setup template is trusted root configuration. Refuse unrelated destination
// files; preserve both old files and absence if validation/reload fails.
func (h Host) setupConfig(source, target string, apply func() error) (result error) {
	data, err := os.ReadFile(filepath.Join(h.TemplatesDir, source))
	if err != nil && !h.DryRun {
		return err
	}
	if h.DryRun {
		h.say("Would apply %s to %s and validate it", source, target)
		return apply()
	}
	old, err := h.read(target)
	existed := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if existed {
		info, err := os.Lstat(h.path(target))
		if err != nil || !info.Mode().IsRegular() || (!bytes.HasPrefix(old, []byte("# Managed by abr")) && !bytes.HasPrefix(old, []byte("; Managed by abr")) && !bytes.HasPrefix(old, []byte("// Managed by abr"))) {
			return fmt.Errorf("refusing to replace unmanaged setup file %s", target)
		}
	}
	if err := h.write(target, data, 0644); err != nil {
		return err
	}
	defer func() {
		if result != nil {
			var restore error
			if existed {
				restore = h.write(target, old, 0644)
			} else {
				restore = h.removeFile(target)
			}
			result = errors.Join(result, restore)
		}
	}()
	return apply()
}

func (h Host) hardenSSH(admin sshAdmin) error {
	// Package upgrades can remove this runtime directory when SSH is inactive
	// or socket-activated; sshd's syntax check still requires it.
	if err := h.command("install", "-d", "-m", "0755", "-o", "root", "-g", "root", "/run/sshd"); err != nil {
		return err
	}
	const path = "/etc/ssh/sshd_config.d/00-abr-hardening.conf"
	err := h.setupConfig("ssh-hardening.conf.tmpl", path, func() error {
		if err := h.command("/usr/sbin/sshd", "-t"); err != nil {
			return err
		}
		for _, args := range [][]string{{"-T"}, {"-T", "-C", admin.context}, {"-T", "-C", "user=root,host=localhost,addr=127.0.0.1"}} {
			out, err := h.run("Verify effective key-only SSH authentication", Command{Name: "/usr/sbin/sshd", Args: args, Private: true})
			if err != nil {
				return err
			}
			if !h.DryRun {
				values := sshValues(out)
				for key, want := range map[string]string{"pubkeyauthentication": "yes", "authenticationmethods": "publickey", "passwordauthentication": "no", "kbdinteractiveauthentication": "no", "permitrootlogin": "without-password", "permitemptypasswords": "no"} {
					if values[key] != want && !(key == "permitrootlogin" && values[key] == "prohibit-password") {
						return fmt.Errorf("SSH config overrides %s; expected %s, existing authentication preserved", key, want)
					}
				}
			}
		}
		return h.command("systemctl", "try-reload-or-restart", "ssh.service")
	})
	if err != nil && !h.DryRun {
		// The config has been restored; reload it if the failed operation reached
		// the running daemon. Existing connections are preserved by SSH reloads.
		return errors.Join(err, h.command("/usr/sbin/sshd", "-t"), h.command("systemctl", "try-reload-or-restart", "ssh.service"))
	}
	return err
}

func (h Host) sshPorts(explicit int) ([]string, error) {
	ports := map[int]bool{}
	add := func(value string) {
		port, err := strconv.Atoi(value)
		if err == nil && port > 0 && port <= 65535 {
			ports[port] = true
		}
	}
	if explicit != 0 {
		ports[explicit] = true
	}
	out, err := h.run("Discover effective SSH listening ports", Command{Name: "/usr/sbin/sshd", Args: []string{"-T"}, Private: true})
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "port" {
			add(fields[1])
		}
	}
	// Ubuntu can socket-activate SSH; preserve its actual listeners as well as
	// sshd's configured ports and any explicitly forwarded/provider port.
	if out, err := h.run("Discover SSH socket listeners", Command{Name: "systemctl", Args: []string{"show", "ssh.socket", "--property=Listen", "--value"}, Private: true}); err == nil {
		for _, field := range strings.Fields(string(out)) {
			if _, port, err := net.SplitHostPort(field); err == nil {
				add(port)
			}
		}
	}
	if connection := strings.Fields(os.Getenv("SSH_CONNECTION")); len(connection) == 4 && net.ParseIP(connection[2]) != nil {
		add(connection[3])
	}
	if len(ports) == 0 {
		if h.DryRun {
			return []string{"<effective-SSH-port>"}, nil
		}
		return nil, fmt.Errorf("could not discover SSH port; pass --ssh-port explicitly")
	}
	numbers := make([]int, 0, len(ports))
	for port := range ports {
		numbers = append(numbers, port)
	}
	slices.Sort(numbers)
	values := make([]string, 0, len(numbers))
	for _, port := range numbers {
		values = append(values, strconv.Itoa(port))
	}
	return values, nil
}

func (h Host) hardenMySQL() error {
	out, err := h.mysql([]byte("SELECT VERSION();\n"))
	if err != nil {
		return err
	}
	if !h.DryRun && !strings.HasPrefix(strings.TrimSpace(string(out)), "8.4.") {
		return fmt.Errorf("MySQL 8.4 is required; refusing to change an unsupported server")
	}
	// Do not delete an existing database or unrelated account during setup.
	rootHosts := "'localhost'"
	if !h.DryRun {
		if _, err := h.readMySQLAdmin(); err == nil {
			rootHosts += ", '127.0.0.1'"
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	out, err = h.mysql([]byte("SELECT (SELECT COUNT(*) FROM mysql.user WHERE User='' OR (User='root' AND Host NOT IN (" + rootHosts + "))) + (SELECT COUNT(*) FROM information_schema.schemata WHERE SCHEMA_NAME='test');\n"))
	if err != nil {
		return err
	}
	if !h.DryRun && strings.TrimSpace(string(out)) != "0" {
		return fmt.Errorf("existing MySQL anonymous/remote-root accounts or test database require manual review; no accounts/databases deleted")
	}
	// Hostname-based grants stop matching with skip_name_resolve. Refuse rather
	// than silently breaking accounts owned by the administrator.
	out, err = h.mysql([]byte("SELECT COUNT(*) FROM mysql.user WHERE Host<>'localhost' AND Host REGEXP '[a-zA-Z]' AND INET6_ATON(SUBSTRING_INDEX(SUBSTRING_INDEX(Host,'/',1),'%',1)) IS NULL;\n"))
	if err != nil {
		return err
	}
	if !h.DryRun && strings.TrimSpace(string(out)) != "0" {
		return fmt.Errorf("existing MySQL hostname-based accounts require manual review before enabling skip_name_resolve; no accounts changed")
	}
	if _, err := h.mysql([]byte("ALTER USER 'root'@'localhost' IDENTIFIED WITH auth_socket;\n")); err != nil {
		return err
	}
	if err := h.setupConfig("mysql-hardening.cnf.tmpl", "/etc/mysql/mysql.conf.d/zz-abr.cnf", func() error {
		if err := h.command("/usr/sbin/mysqld", "--validate-config", "--user=mysql"); err != nil {
			return err
		}
		return h.command("systemctl", "restart", "mysql")
	}); err != nil {
		return errors.Join(err, h.command("systemctl", "restart", "mysql"))
	}
	out, err = h.mysql([]byte("SELECT (@@bind_address='127.0.0.1' AND @@local_infile=0 AND @@skip_name_resolve=1 AND (SELECT COUNT(*) FROM mysql.user WHERE User='root' AND Host='localhost' AND plugin='auth_socket')=1);\n"))
	if err != nil {
		return err
	}
	if !h.DryRun && strings.TrimSpace(string(out)) != "1" {
		return fmt.Errorf("MySQL security settings could not be verified")
	}
	out, err = h.mysql([]byte("SELECT COUNT(*) FROM mysql.component WHERE component_urn='file://component_validate_password';\n"))
	if err != nil {
		return err
	}
	if h.DryRun || strings.TrimSpace(string(out)) == "0" {
		if _, err := h.mysql([]byte("INSTALL COMPONENT 'file://component_validate_password';\n")); err != nil {
			return err
		}
	} else if strings.TrimSpace(string(out)) != "1" {
		return fmt.Errorf("unexpected MySQL password validation component state")
	}
	_, err = h.mysql([]byte("SET PERSIST validate_password.policy=2;\nSET PERSIST validate_password.length=14;\n"))
	return err
}
