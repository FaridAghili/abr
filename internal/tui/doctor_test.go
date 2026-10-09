package tui

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestDoctorTUIShowsDiagnosticOutcome(t *testing.T) {
	for _, failed := range []bool{false, true} {
		o := testOptions(t)
		o.RunCommand = func(args []string, out io.Writer) error {
			if strings.Join(args, " ") != "doctor" {
				return fmt.Errorf("wrong diagnostic: %v", args)
			}
			if failed {
				io.WriteString(out, "ERROR: app/nuxt-http at 10000: port conflict\n")
				return errors.New("checks found problems")
			}
			io.WriteString(out, "OK: app/nuxt-http at 10000: listening, owned by active abr-app-nuxt.service\n")
			return nil
		}
		m := newModel(o)
		m.toolsMenu()
		for range 6 {
			press(m, tea.KeyDown)
		}
		m.next()
		finishLogView(t, m)
		want := "Checks passed"
		if failed {
			want = "Checks need attention"
		}
		if view := m.View().Content; !strings.Contains(view, want) || strings.Contains(view, "Command failed") {
			t.Fatalf("misleading diagnostic view: %s", view)
		}
	}
}
