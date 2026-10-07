package tui

import (
	"strings"
	"testing"

	"abr/internal/config"
)

func TestEditFormsPrefillAndGuideDeployment(t *testing.T) {
	m := newModel(testOptions(t))
	a := config.App{Name: "app", Type: "laravel", Domain: "current.example.com", Aliases: []string{"old.example.com"}, HealthCheck: "https://current.example.com/up", Queue: config.Queue{Workers: 3}, Scheduler: config.Component{Enabled: true}, Database: config.Database{Enabled: true}, Web: config.Web{Driver: "fpm"}}
	for _, check := range []struct{ section, value string }{
		{"domains", "current.example.com"}, {"workers", "--queue-workers 3"}, {"health", "https://current.example.com/up"}, {"components", "--scheduler=true"},
	} {
		m.editForm(a, check.section)
		if !strings.Contains(m.form.View(), "Example:") {
			t.Fatal("missing description/example")
		}
		m.next()
		if !strings.Contains(strings.Join(m.current.args, " "), check.value) || m.page != "confirm" || m.approved || !strings.Contains(m.reviewText, "Deploy afterward") {
			t.Fatalf("edit lost defaults or skipped guided review: %v %s", m.current.args, m.reviewText)
		}
	}
	if !strings.Contains(nextStep([]string{"edit", "app"}), "Deploy") {
		t.Fatal("missing deployment next step")
	}
}
