package host

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type mysqlAdminCredentials struct {
	User     string `json:"user"`
	Host     string `json:"host"`
	Password string `json:"password"`
	Ready    bool   `json:"ready"`
}

// Server credentials live outside app records, so even full app removal retains them.
func (h Host) mysqlAdminPath() string {
	return filepath.Join(h.Manager.StateDir, "mysql-admin.json")
}

func (h Host) readMySQLAdmin() (mysqlAdminCredentials, error) {
	var c mysqlAdminCredentials
	data, err := h.read(h.mysqlAdminPath())
	if err != nil {
		return c, err
	}
	info, err := os.Lstat(h.path(h.mysqlAdminPath()))
	if err != nil {
		return c, err
	}
	if info.Mode().Perm()&0077 != 0 {
		return c, fmt.Errorf("MySQL admin credentials must be private (mode 0600)")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid MySQL admin ownership record: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF || c.User != "root" || c.Host != "127.0.0.1" || !regexp.MustCompile(`^[a-f0-9]{64}Aa1!$`).MatchString(c.Password) {
		return c, fmt.Errorf("invalid MySQL admin ownership record")
	}
	return c, nil
}

func (h Host) configureMySQLAdmin() error {
	if h.DryRun {
		h.say("Would create/verify password-based root@127.0.0.1 for TablePlus over SSH; save credentials privately")
		return nil
	}
	c, err := h.readMySQLAdmin()
	if os.IsNotExist(err) {
		out, err := h.mysql([]byte("SELECT COUNT(*) FROM mysql.user WHERE User='root' AND Host='127.0.0.1';\n"))
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(out)) != "0" {
			return fmt.Errorf("root@127.0.0.1 already exists without an abr ownership record; existing password preserved")
		}
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return err
		}
		c = mysqlAdminCredentials{User: "root", Host: "127.0.0.1", Password: hex.EncodeToString(secret) + "Aa1!"}
		data, _ := json.MarshalIndent(c, "", "  ")
		// Persist intent first: retries after partial SQL failure reuse the same password.
		if err := h.write(h.mysqlAdminPath(), data, 0600); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if !c.Ready {
		sql := fmt.Sprintf("CREATE USER IF NOT EXISTS 'root'@'127.0.0.1' IDENTIFIED BY '%s';\n", c.Password)
		if _, err := h.mysql([]byte(sql)); err != nil {
			return err
		}
		// Authenticate before granting privileges; never adopt an unrelated password.
		if err := h.verifyMySQLAdmin(c); err != nil {
			return err
		}
		if _, err := h.mysql([]byte("GRANT ALL PRIVILEGES ON *.* TO 'root'@'127.0.0.1' WITH GRANT OPTION;\n")); err != nil {
			return err
		}
		if err := h.verifyMySQLAdminPrivileges(); err != nil {
			return err
		}
		c.Ready = true
		data, _ := json.MarshalIndent(c, "", "  ")
		if err := h.write(h.mysqlAdminPath(), data, 0600); err != nil {
			return err
		}
	} else if err := h.verifyMySQLAdmin(c); err != nil {
		return err
	} else if err := h.verifyMySQLAdminPrivileges(); err != nil {
		return err
	}
	return h.showMySQLAdmin(c, false)
}

func (h Host) verifyMySQLAdminPrivileges() error {
	out, err := h.mysql([]byte("SELECT COUNT(*) FROM mysql.user WHERE User='root' AND Host='127.0.0.1' AND Select_priv='Y' AND Create_priv='Y' AND Create_user_priv='Y' AND Grant_priv='Y';\n"))
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) != "1" {
		return fmt.Errorf("MySQL admin privileges could not be verified; no password changed")
	}
	return nil
}

func (h Host) verifyMySQLAdmin(c mysqlAdminCredentials) error {
	out, err := h.mysqlTCP(c.User, c.Password, "", []byte("SELECT CURRENT_USER();\n"))
	if err != nil {
		return fmt.Errorf("MySQL admin login failed; saved password preserved: %w", err)
	}
	if strings.TrimSpace(string(out)) != "root@127.0.0.1" {
		return fmt.Errorf("MySQL admin login matched an unexpected account")
	}
	return nil
}

// Authentication data is passed through a private file, never arguments or logs.
func (h Host) mysqlTCP(user, password, database string, sql []byte) ([]byte, error) {
	file, err := os.CreateTemp(h.path(h.Manager.StateDir), ".mysql-admin-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	text := fmt.Sprintf("[client]\nuser=%s\npassword=%s\nprotocol=TCP\nhost=127.0.0.1\nport=3306\n", user, password)
	if _, err := io.WriteString(file, text); err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	args := []string{"--defaults-file=" + file.Name(), "--no-login-paths", "--batch", "--skip-column-names", "--connect-timeout=5"}
	if database != "" {
		args = append(args, "--database="+database)
	}
	return h.run("Verify MySQL access over loopback TCP", Command{Name: "mysql", Args: args, Input: sql, Private: true})
}

// MySQLAdmin only reads the server account saved by setup. Password display is explicit.
func (h Host) MySQLAdmin(show bool) error {
	return h.locked(func() error {
		if h.DryRun {
			h.say("Would read MySQL admin connection details; no password displayed")
			return nil
		}
		c, err := h.readMySQLAdmin()
		if os.IsNotExist(err) {
			return fmt.Errorf("MySQL admin login is not configured; run abr setup first")
		}
		if err != nil {
			return err
		}
		if !c.Ready {
			return fmt.Errorf("MySQL admin setup is incomplete; rerun abr setup")
		}
		return h.showMySQLAdmin(c, show)
	})
}

func (h Host) showMySQLAdmin(c mysqlAdminCredentials, show bool) error {
	h.say("TablePlus · MySQL via SSH\nDatabase host: 127.0.0.1\nDatabase port: 3306\nDatabase user: root\nDatabase: leave blank to browse all databases\nSSH: your VPS address, SSH port, administrator user and SSH key")
	if show {
		h.say("Database password: %s", c.Password)
	} else {
		h.say("Admin credentials saved privately in %s; view with abr database --admin --show", h.mysqlAdminPath())
	}
	return nil
}
