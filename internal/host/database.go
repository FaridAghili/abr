package host

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"abr/internal/config"
)

type credentials struct {
	App        string `json:"app"`
	Database   string `json:"database"`
	User       string `json:"user"`
	Password   string `json:"password"`
	Ready      bool   `json:"ready"`
	TCPManaged bool   `json:"tcp_managed"`
	TCPReady   bool   `json:"tcp_ready"`
}

func databaseName(name string) string {
	return strings.ReplaceAll(name, "-", "_")
}

// MySQL account names are limited to 32 characters, while database names can
// use the full app name. Keep a hash suffix to distinguish long names.
func databaseUser(name string) string {
	database := databaseName(name)
	if len(database) <= 32 {
		return database
	}
	sum := sha256.Sum256([]byte(database))
	return fmt.Sprintf("%s_%x", database[:23], sum[:4])
}

func (h Host) credentialsPath(name string) string {
	return filepath.Join(h.Manager.StateDir, "databases", name+".json")
}
func (h Host) credentialsEnvPath(name string) string {
	return filepath.Join(h.Manager.StateDir, "credentials", name+".env")
}

func (h Host) Database(name string, show bool) error {
	return h.locked(func() error {
		a, _, err := h.application(name)
		if err != nil {
			return err
		}
		return h.database(a, show)
	})
}

func (h Host) database(a config.App, show bool) error {
	if !a.Database.Enabled {
		return fmt.Errorf("%s: managed database is disabled", a.Name)
	}
	if h.DryRun {
		h.say("Would create/verify a dedicated MySQL database and local socket/127.0.0.1 accounts for %s; save credentials privately", a.Name)
		return nil
	}
	path := h.credentialsPath(a.Name)
	saved, err := h.read(path)
	c := credentials{App: a.Name, Database: databaseName(a.Name), User: databaseUser(a.Name)}
	if err == nil {
		if err := json.Unmarshal(saved, &c); err != nil {
			return fmt.Errorf("corrupt database credentials: %w", err)
		}
		if c.App != a.Name || c.Database != databaseName(a.Name) || c.User != databaseUser(a.Name) || !regexp.MustCompile(`^[a-f0-9]{64}Aa1!$`).MatchString(c.Password) {
			return fmt.Errorf("invalid database ownership record for %s", a.Name)
		}
	} else if os.IsNotExist(err) {
		// Never adopt an existing database/account, and never change its password.
		query := fmt.Sprintf("SELECT (SELECT COUNT(*) FROM mysql.user WHERE User='%s' AND Host='localhost') + (SELECT COUNT(*) FROM information_schema.schemata WHERE SCHEMA_NAME='%s');\n", c.User, c.Database)
		result, err := h.mysql([]byte(query))
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(result)) != "0" {
			return fmt.Errorf("database or MySQL user already exists; use --no-database and manage its .env credentials yourself")
		}
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return err
		}
		c.Password = hex.EncodeToString(secret) + "Aa1!"
		data, _ := json.MarshalIndent(c, "", "  ")
		// Keep the same credentials after a partial SQL failure or process interruption.
		if err := h.write(path, data, 0600); err != nil {
			return err
		}
	} else {
		return err
	}
	if !c.Ready {
		sql := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;\nCREATE USER IF NOT EXISTS '%s'@'localhost' IDENTIFIED BY '%s';\n", c.Database, c.User, c.Password)
		if _, err := h.mysql([]byte(sql)); err != nil {
			return err
		}
		if err := h.databaseGrants(c, "localhost"); err != nil {
			return err
		}
		c.Ready = true
		data, _ := json.MarshalIndent(c, "", "  ")
		if err := h.write(path, data, 0600); err != nil {
			return err
		}
	} else {
		query := fmt.Sprintf("SELECT (SELECT COUNT(*) FROM mysql.user WHERE User='%s' AND Host='localhost') + (SELECT COUNT(*) FROM information_schema.schemata WHERE SCHEMA_NAME='%s');\n", c.User, c.Database)
		result, err := h.mysql([]byte(query))
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(result)) != "2" {
			return fmt.Errorf("recorded MySQL database/account is missing; restore it before deploying %s", a.Name)
		}
	}
	// Socket and TCP identities are separate when skip_name_resolve is enabled.
	// Record ownership before creation and never adopt an unrecorded TCP account.
	if !c.TCPManaged {
		out, err := h.mysql([]byte(fmt.Sprintf("SELECT COUNT(*) FROM mysql.user WHERE User='%s' AND Host='127.0.0.1';\n", c.User)))
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(out)) != "0" {
			return fmt.Errorf("unrecorded MySQL account %s@127.0.0.1 already exists; no password changed", c.User)
		}
		c.TCPManaged = true
		data, _ := json.MarshalIndent(c, "", "  ")
		if err := h.write(path, data, 0600); err != nil {
			return err
		}
	}
	if !c.TCPReady {
		if _, err := h.mysql([]byte(fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'127.0.0.1' IDENTIFIED BY '%s';\n", c.User, c.Password))); err != nil {
			return err
		}
	}
	out, err := h.mysqlTCP(c.User, c.Password, "", []byte("SELECT CURRENT_USER();\n"))
	if err != nil {
		return fmt.Errorf("managed MySQL loopback login failed: %w", err)
	}
	if strings.TrimSpace(string(out)) != c.User+"@127.0.0.1" {
		return fmt.Errorf("managed MySQL loopback login matched an unexpected account")
	}
	if !c.TCPReady {
		if err := h.databaseGrants(c, "127.0.0.1"); err != nil {
			return err
		}
	}
	if _, err := h.mysqlTCP(c.User, c.Password, c.Database, []byte("SELECT 1;\n")); err != nil {
		return fmt.Errorf("managed MySQL database access failed: %w", err)
	}
	if !c.TCPReady {
		c.TCPReady = true
		data, _ := json.MarshalIndent(c, "", "  ")
		if err := h.write(path, data, 0600); err != nil {
			return err
		}
	}
	env := fmt.Sprintf("DB_CONNECTION=mysql\nDB_HOST=127.0.0.1\nDB_PORT=3306\nDB_DATABASE=%s\nDB_USERNAME=%s\nDB_PASSWORD=%s\n", c.Database, c.User, c.Password)
	if err := h.write(h.credentialsEnvPath(a.Name), []byte(env), 0600); err != nil {
		return err
	}
	if show {
		h.say("%s", strings.TrimSuffix(env, "\n"))
	} else {
		h.say("MySQL ready for %s; copy credentials from %s into the project's .env", a.Name, h.credentialsEnvPath(a.Name))
	}
	return nil
}

// Scope newly provisioned accounts to one literal database. MySQL interprets
// underscores as wildcards unless partial_revokes is enabled.
func (h Host) databaseGrants(c credentials, host string) error {
	out, err := h.mysql([]byte("SELECT @@partial_revokes;\n"))
	if err != nil {
		return err
	}
	database := c.Database
	switch strings.TrimSpace(string(out)) {
	case "0":
		database = strings.NewReplacer("_", `\_`, "%", `\%`).Replace(database)
	case "1":
	default:
		return fmt.Errorf("unexpected MySQL partial_revokes setting")
	}
	sql := fmt.Sprintf("GRANT ALL PRIVILEGES ON `%s`.* TO '%s'@'%s';\n", database, c.User, host)
	_, err = h.mysql([]byte(sql))
	return err
}

func (h Host) mysql(sql []byte) ([]byte, error) {
	return h.run("Execute private MySQL account/database statements over the local root socket", Command{
		Name: "mysql", Args: []string{"--protocol=socket", "--user=root", "--batch", "--skip-column-names"}, Input: sql, Private: true,
	})
}
