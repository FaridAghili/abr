package tui

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

func TestArtisanShellMenuAndReturn(t *testing.T) {
	o := testOptions(t)
	a := navigationApp("example")
	saveApps(t, o, a)
	called := false
	o.ArtisanShell = func(name string) (*exec.Cmd, error) {
		called = name == a.Name
		return exec.Command("/bin/bash"), nil
	}
	m := newModel(o)
	m.width, m.height = 100, 45
	m.appMenu(a)
	if !strings.Contains(m.View().Content, "Artisan shell") {
		t.Fatal("Laravel action missing")
	}
	if m.appAction(a, "artisan-shell") == nil || !called || !m.busy {
		t.Fatal("foreground shell not prepared")
	}
	m.Update(artisanShellFinished{name: a.Name})
	if m.title != a.Name || m.busy {
		t.Fatal("did not return to app menu")
	}
	m.Update(artisanShellFinished{name: a.Name, err: fmt.Errorf("failed")})
	if m.result == nil || m.busy {
		t.Fatal("failure not shown")
	}
	a.Type = "nuxt"
	m.appMenu(a)
	if strings.Contains(m.View().Content, "Artisan shell") {
		t.Fatal("Nuxt offers Artisan")
	}
	called = false
	m.appAction(a, "artisan-shell")
	if called || m.result == nil {
		t.Fatal("Nuxt started shell")
	}
}
