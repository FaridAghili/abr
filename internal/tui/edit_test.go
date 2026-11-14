package tui

import (
	"strings"
	"testing"

	"abr/internal/config"
	tea "charm.land/bubbletea/v2"
)

func TestEditFormsPrefillAndGuideDeployment(t *testing.T) {
	m := newModel(testOptions(t))
	a := config.App{Name: "app", Type: "laravel", Domain: "current.example.com", Aliases: []string{"old.example.com"}, HealthCheck: "https://current.example.com/up", Queue: config.Queue{Workers: 3}, Scheduler: config.Component{Enabled: true}, Database: config.Database{Enabled: true}, Web: config.Web{Driver: "fpm"}}
	for _, check := range []struct{ section, value string }{
		{"domains", "current.example.com"}, {"workers", "--queue-workers 3"}, {"health", "https://current.example.com/up"}, {"components", "--scheduler=true"}, {"build-order", "--build-order frontend-first"},
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

func TestEditBuildOrderSelectsAndPrefillsComposerFirst(t *testing.T) {
	m := newModel(testOptions(t))
	a := config.App{Name: "app", Type: "laravel"}
	m.editForm(a, "build-order")
	press(m, tea.KeyDown)
	m.next()
	if strings.Join(m.current.args, " ") != "edit app --build-order composer-first" {
		t.Fatalf("build selection lost: %v", m.current.args)
	}
	a.BuildOrder = config.BuildComposerFirst
	m.editForm(a, "build-order")
	m.next()
	if strings.Join(m.current.args, " ") != "edit app --build-order composer-first" {
		t.Fatalf("saved build order lost: %v", m.current.args)
	}
}

func TestRegistrationAdvancedBuildOrder(t *testing.T) {
	m := newModel(testOptions(t))
	m.registrationForm("app", "laravel", true)
	m.Update(tea.PasteMsg{Content: "example.com"})
	for step := 0; !strings.Contains(m.form.View(), "Advanced settings?"); step++ {
		if step > 12 {
			t.Fatal("advanced settings inaccessible")
		}
		m.form.NextGroup()
	}
	press(m, tea.KeyDown)
	m.form.NextGroup()
	m.form.NextGroup()
	if !strings.Contains(m.form.View(), "Laravel build order") {
		t.Fatal("build order inaccessible")
	}
	press(m, tea.KeyDown)
	m.next()
	if !strings.Contains(strings.Join(m.current.args, " "), "--build-order composer-first") {
		t.Fatalf("registration lost build order: %v", m.current.args)
	}
}

func TestEmbeddingFormPrefillsAndDisables(t *testing.T) {
	m := newModel(testOptions(t))
	a := config.App{Name: "app", Type: "nuxt", Embedding: config.Embedding{Paths: []string{"/banner.html", "/ads/*"}, Origins: []string{"*"}}}
	m.editForm(a, "embedding")
	m.next()
	if strings.Join(m.current.args, " ") != "edit app --embed-path /banner.html --embed-path /ads/* --embed-origin *" {
		t.Fatalf("lost embedding values: %v", m.current.args)
	}
	a.Embedding = config.Embedding{Origins: []string{"*"}}
	m.editForm(a, "embedding")
	m.next()
	if len(m.current.args) != 6 || m.current.args[3] != "" || m.current.args[5] != "" {
		t.Fatalf("disable did not clear both lists: %v", m.current.args)
	}
}

func TestEditReverbDomainPrefillsAndDisables(t *testing.T) {
	m := newModel(testOptions(t))
	a := config.App{Name: "app", Type: "laravel", Web: config.Web{Driver: "fpm"}, Reverb: config.Reverb{Domain: "ws.example.com"}}
	m.editForm(a, "reverb")
	if !strings.Contains(m.form.View(), "Example: ws.example.com") {
		t.Fatal("missing description/example")
	}
	m.next()
	if strings.Join(m.current.args, " ") != "edit app --reverb-domain ws.example.com" || !strings.Contains(m.reviewText, "Deploy afterward") {
		t.Fatalf("lost Reverb domain: %v", m.current.args)
	}
	a.Reverb.Domain = ""
	m.editForm(a, "reverb")
	m.next()
	if len(m.current.args) != 4 || m.current.args[2] != "--reverb-domain" || m.current.args[3] != "" {
		t.Fatalf("clearing did not disable Reverb: %q", m.current.args)
	}
	if err := reverbDomainValid(""); err == nil {
		t.Fatal("registration accepted an empty Reverb domain")
	}
	if err := reverbDomainValid("-ws.example.com"); err == nil {
		t.Fatal("invalid Reverb domain accepted")
	}
}

func TestRegistrationAdvancedReverb(t *testing.T) {
	m := newModel(testOptions(t))
	m.registrationForm("app", "laravel", true)
	m.Update(tea.PasteMsg{Content: "example.com"})
	for step := 0; !strings.Contains(m.form.View(), "Advanced settings?"); step++ {
		if step > 12 {
			t.Fatal("advanced settings inaccessible")
		}
		m.form.NextGroup()
	}
	press(m, tea.KeyDown)
	m.form.NextGroup()
	if !strings.Contains(m.form.View(), "Reverb · WebSockets") {
		t.Fatal("Reverb option missing")
	}
	for range 3 {
		press(m, tea.KeyDown)
	}
	press(m, tea.KeySpace)
	m.form.NextGroup()
	if !strings.Contains(m.form.View(), "Reverb domain") {
		t.Fatalf("Reverb domain step missing:\n%s", m.form.View())
	}
	m.Update(tea.PasteMsg{Content: "ws.example.com"})
	m.next()
	args := strings.Join(m.current.args, " ")
	if !strings.Contains(args, "--reverb-domain ws.example.com") || strings.Contains(args, "--reverb ") {
		t.Fatalf("registration lost Reverb domain: %v", m.current.args)
	}
}
