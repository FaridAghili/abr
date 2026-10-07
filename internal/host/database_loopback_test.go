package host

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestManagedDatabaseSupportsLoopbackAndPreservesSocketCredentials(t *testing.T) {
	h, runner, out, a := fixture(t)
	a.Database.Enabled = true
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	c, err := h.transferCredentials(a)
	if err != nil || !c.TCPManaged || !c.TCPReady {
		t.Fatalf("TCP credentials not ready: %v", err)
	}
	data, err := h.read(h.credentialsEnvPath(a.Name))
	if err != nil || dotenvValue(data, "DB_HOST") != "127.0.0.1" {
		t.Fatal("generated credentials do not prefer loopback")
	}
	if err := os.WriteFile(a.Directory+"/.env", data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.laravelEnv(a); err != nil {
		t.Fatalf("loopback env refused: %v", err)
	}
	// Socket credentials already recorded for an app retain the same password
	// when the additional, explicitly tracked TCP identity is provisioned.
	password := c.Password
	c.TCPManaged, c.TCPReady = false, false
	data, _ = json.Marshal(c)
	if err := h.write(h.credentialsPath(a.Name), data, 0600); err != nil {
		t.Fatal(err)
	}
	runner.tcp = false
	if err := h.Database(a.Name, false); err != nil {
		t.Fatal(err)
	}
	c, err = h.transferCredentials(a)
	if err != nil || c.Password != password || !c.TCPReady {
		t.Fatal("existing password changed")
	}
	if strings.Contains(out.String(), password) {
		t.Fatal("password leaked")
	}
}

func TestLoopbackAccountIsNeverAdoptedAndInterruptedProvisioningRetries(t *testing.T) {
	h, runner, _, a := fixture(t)
	a.Database.Enabled = true
	if _, err := h.Register(a, nil); err != nil {
		t.Fatal(err)
	}
	c, _ := h.transferCredentials(a)
	c.TCPManaged, c.TCPReady = false, false
	data, _ := json.Marshal(c)
	if err := h.write(h.credentialsPath(a.Name), data, 0600); err != nil {
		t.Fatal(err)
	}
	runner.calls = nil
	if err := h.Database(a.Name, false); err == nil || !strings.Contains(err.Error(), "unrecorded MySQL account") {
		t.Fatalf("adopted existing TCP account: %v", err)
	}
	for _, cmd := range runner.calls {
		if strings.Contains(string(cmd.Input), "CREATE USER") || strings.Contains(string(cmd.Input), "GRANT") {
			t.Fatal("modified unrecorded account")
		}
	}
	runner.tcp = false
	runner.fail = func(cmd Command) error {
		if strings.Contains(string(cmd.Input), "GRANT") && strings.Contains(string(cmd.Input), "@'127.0.0.1'") {
			return testExit(1)
		}
		return nil
	}
	if err := h.Database(a.Name, false); err == nil {
		t.Fatal("failed TCP grant reported success")
	}
	data, _ = h.read(h.credentialsPath(a.Name))
	var pending credentials
	json.Unmarshal(data, &pending)
	if !pending.TCPManaged || pending.TCPReady || pending.Password != c.Password {
		t.Fatal("failed TCP grant lost ownership or secret")
	}
	runner.fail = nil
	if err := h.Database(a.Name, false); err != nil {
		t.Fatal(err)
	}
	finished, _ := h.transferCredentials(a)
	if !finished.TCPReady || finished.Password != c.Password {
		t.Fatal("retry changed secret or failed to finish")
	}
}
