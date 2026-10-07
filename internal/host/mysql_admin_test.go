package host

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type adminRunner struct {
	*fakeRunner
	t                   *testing.T
	exists, granted     bool
	failGrant, badLogin bool
}

func (r *adminRunner) Run(c Command) ([]byte, error) {
	if c.Name != "mysql" {
		return r.fakeRunner.Run(c)
	}
	r.calls = append(r.calls, c)
	if !c.Private {
		r.t.Fatal("admin SQL was not private")
	}
	sql := string(c.Input)
	switch {
	case strings.HasPrefix(sql, "CREATE USER"):
		r.exists = true
	case strings.HasPrefix(sql, "GRANT ALL"):
		if r.failGrant {
			r.failGrant = false
			return nil, testExit(1)
		}
		r.granted = true
	case sql == "SELECT CURRENT_USER();\n":
		if len(c.Args) == 0 || !strings.HasPrefix(c.Args[0], "--defaults-file=") {
			r.t.Fatal("TCP login did not use private options file")
		}
		path := strings.TrimPrefix(c.Args[0], "--defaults-file=")
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			r.t.Fatal("unsafe temporary client credentials")
		}
		data, _ := os.ReadFile(path)
		if !bytes.Contains(data, []byte("protocol=TCP\nhost=127.0.0.1\n")) {
			r.t.Fatal("admin authentication did not use loopback TCP")
		}
		if r.badLogin {
			return nil, testExit(1)
		}
		return []byte("root@127.0.0.1\n"), nil
	case strings.Contains(sql, "Select_priv='Y'"):
		return []byte(fmt.Sprintf("%d\n", boolInt(r.granted))), nil
	case strings.HasPrefix(sql, "SELECT COUNT(*) FROM mysql.user"):
		return []byte(fmt.Sprintf("%d\n", boolInt(r.exists))), nil
	}
	return nil, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestMySQLAdminPrivateStableAndRecoversPartialSetup(t *testing.T) {
	h, fake, out, _ := fixture(t)
	r := &adminRunner{fakeRunner: fake, t: t, failGrant: true}
	h.Runner = r
	if err := h.locked(h.configureMySQLAdmin); err == nil {
		t.Fatal("failed grant reported success")
	}
	before, err := h.readMySQLAdmin()
	if err != nil || before.Ready || before.Password == "" {
		t.Fatalf("missing retry credentials: %v", err)
	}
	if err := h.MySQLAdmin(true); err == nil {
		t.Fatal("showed incomplete credentials")
	}
	if err := h.locked(h.configureMySQLAdmin); err != nil {
		t.Fatal(err)
	}
	after, err := h.readMySQLAdmin()
	if err != nil || !after.Ready || after.Password != before.Password {
		t.Fatal("partial setup rotated admin password")
	}
	callCount := len(r.calls)
	if err := h.locked(h.configureMySQLAdmin); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.calls[callCount:] {
		if strings.Contains(string(c.Input), "CREATE USER") || strings.Contains(string(c.Input), "GRANT ") || strings.Contains(string(c.Input), "ALTER USER") {
			t.Fatal("repeated setup modified the existing account")
		}
	}
	if strings.Contains(out.String(), after.Password) {
		t.Fatal("setup leaked admin password")
	}
	for _, c := range r.calls {
		if strings.Contains(strings.Join(c.Args, " "), after.Password) || strings.Contains(strings.Join(c.Env, " "), after.Password) {
			t.Fatal("password leaked into process arguments/environment")
		}
	}
	files, _ := filepath.Glob(filepath.Join(h.Manager.StateDir, ".mysql-admin-*"))
	if len(files) != 0 {
		t.Fatal("retained temporary client credentials")
	}
	if err := h.MySQLAdmin(true); err != nil || !strings.Contains(out.String(), "Database password: "+after.Password) {
		t.Fatalf("explicit credential display failed: %v", err)
	}
	r.granted = false
	if err := h.locked(h.configureMySQLAdmin); err == nil {
		t.Fatal("missing global privileges reported as ready")
	}
	if err := os.Chmod(h.mysqlAdminPath(), 0644); err != nil {
		t.Fatal(err)
	}
	if err := h.MySQLAdmin(true); err == nil {
		t.Fatal("accepted exposed admin credentials")
	}
}

func TestMySQLAdminNeverAdoptsExistingAccountOrGrantsWrongLogin(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			h, fake, out, _ := fixture(t)
			r := &adminRunner{fakeRunner: fake, t: t, exists: existing, badLogin: true}
			h.Runner = r
			if err := h.locked(h.configureMySQLAdmin); err == nil {
				t.Fatal("unsafe admin setup succeeded")
			}
			if r.granted || strings.Contains(out.String(), "saved privately") {
				t.Fatal("failed login received admin privileges or reported success")
			}
			for _, c := range r.calls {
				if strings.Contains(string(c.Input), "ALTER USER") || (existing && strings.HasPrefix(string(c.Input), "CREATE USER")) {
					t.Fatal("modified an unrelated root account")
				}
			}
		})
	}
}

func TestMySQLAdminRejectsCorruptRecordAndDryRunDoesNotReadSecrets(t *testing.T) {
	h, r, out, _ := fixture(t)
	if err := h.write(h.mysqlAdminPath(), []byte(`{"user":"root","host":"%","password":"unsafe","ready":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.MySQLAdmin(true); err == nil {
		t.Fatal("invalid ownership record accepted")
	}
	h.DryRun = true
	if err := h.MySQLAdmin(true); err != nil {
		t.Fatal(err)
	}
	if err := h.configureMySQLAdmin(); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 0 || strings.Contains(out.String(), "unsafe") {
		t.Fatal("dry run contacted MySQL or read secrets")
	}
}
