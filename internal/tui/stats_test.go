package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"abr/internal/resources"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func usageFixture() resources.Report {
	return resources.Report{MeasuredAt: time.Now(), Host: resources.Host{Cores: 2, CPUPercent: 42, MemoryTotal: 4 << 30, MemoryUsed: 2 << 30, MemoryPercent: 50, SwapTotal: 1 << 30}, Apps: []resources.App{
		{Name: "light", Kind: "app", Known: true, CPUPercent: 1, MemoryBytes: 50 << 20, MemoryPercent: 1, Processes: 1, Services: []resources.Service{{Name: "php8.5-fpm", MemoryBytes: 50 << 20, Processes: 1}}},
		{Name: "heavy", Kind: "app", Known: true, CPUPercent: 70, MemoryBytes: 1500 << 20, MemoryPercent: 37, Processes: 9, Services: []resources.Service{{Name: "abr-heavy-queue@1", CPUPercent: 60, MemoryBytes: 1 << 30, Processes: 1}, {Name: "php8.5-fpm", CPUPercent: 10, MemoryBytes: 500 << 20, Processes: 8}}},
		{Name: "new", Kind: "app"},
		{Name: "MySQL", Kind: "shared", Known: true, MemoryBytes: 400 << 20, MemoryPercent: 10, Processes: 1},
	}}
}

// deliver runs a command and feeds its message back, like the Bubble Tea loop.
func deliver(t *testing.T, m *model, cmd tea.Cmd) {
	t.Helper()
	for _, msg := range flatten(cmd) {
		if _, ok := msg.(statsMsg); ok {
			m.Update(msg)
			return
		}
	}
	t.Fatal("no measurement was requested")
}

func flatten(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, flatten(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

func TestResourceMonitorRefreshesSortsAndOpensServices(t *testing.T) {
	o := testOptions(t)
	saveApps(t, o, navigationApp("light"), navigationApp("heavy"))
	var calls []string
	var previous []*resources.Sample
	o.Usage = func(name string, p *resources.Sample) (resources.Report, resources.Sample, error) {
		calls, previous = append(calls, name), append(previous, p)
		r := usageFixture()
		if name != "" {
			for _, a := range r.Apps {
				if a.Name == name {
					r.Apps = []resources.App{a}
				}
			}
		}
		return r, resources.Sample{At: time.Now()}, nil
	}
	m := newModel(o)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	press(m, tea.KeyDown)
	press(m, tea.KeyDown)
	deliver(t, m, m.next())
	view := viewText(m)
	if m.page != "stats" || !strings.Contains(view, "Live") || !strings.Contains(view, "SHARED") || !strings.Contains(view, "Ubuntu user not created") || !strings.Contains(view, "▲") {
		t.Fatalf("monitor: %s", view)
	}
	// Memory sort puts the heaviest app first; shared services stay below apps.
	if strings.Index(view, "heavy") > strings.Index(view, "light") || strings.Index(view, "MySQL") < strings.Index(view, "light") {
		t.Fatalf("rows not sorted by memory: %s", view)
	}
	press(m, 's')
	press(m, 's')
	if view := viewText(m); !strings.Contains(view, "sort: name") || strings.Index(view, "heavy") > strings.Index(view, "light") {
		t.Fatalf("name sort: %s", view)
	}
	// A timer tick measures again from the previous sample.
	_, cmd := m.Update(statsTick{generation: m.generation})
	deliver(t, m, cmd)
	if len(calls) != 2 || previous[0] != nil || previous[1] == nil {
		t.Fatalf("refresh did not reuse the sample: %v %v", calls, previous)
	}
	deliver(t, m, press(m, tea.KeyEnter))
	if m.title != "heavy / Resource usage" || !strings.Contains(viewText(m), "queue@1") || !strings.Contains(viewText(m), "web (PHP-FPM)") {
		t.Fatalf("service view: %s", viewText(m))
	}
	press(m, tea.KeyEscape)
	if m.page != "stats" || m.stats.name != "" || m.stats.sort != 2 {
		t.Fatal("service view did not return to the monitor")
	}
	generation := m.generation
	press(m, tea.KeyEscape)
	if m.page != "home" {
		t.Fatal("monitor did not return to the main menu")
	}
	// Measurements from a monitor that was left neither render nor schedule more work.
	if _, cmd := m.Update(statsTick{generation: generation}); cmd != nil && m.page != "home" {
		t.Fatal("stale timer reopened the monitor")
	}
	if _, cmd := m.Update(statsMsg{generation: generation}); m.page != "home" || cmd != nil && len(flatten(cmd)) > 0 {
		t.Fatal("stale measurement kept refreshing")
	}
}

func TestResourceMonitorShowsErrorsAndFitsTerminal(t *testing.T) {
	o := testOptions(t)
	o.Usage = func(string, *resources.Sample) (resources.Report, resources.Sample, error) {
		return resources.Report{}, resources.Sample{}, errors.New("live resource usage reads /proc on the VPS")
	}
	m := newModel(o)
	deliver(t, m, m.statsMonitor(""))
	if view := viewText(m); !strings.Contains(view, "Cannot read resource usage") || !strings.Contains(view, "/proc") {
		t.Fatalf("error not shown: %s", view)
	}
	o.Usage = func(string, *resources.Sample) (resources.Report, resources.Sample, error) {
		return usageFixture(), resources.Sample{At: time.Now()}, nil
	}
	for _, name := range []string{"", "heavy"} {
		m = newModel(o)
		deliver(t, m, m.statsMonitor(name))
		for _, size := range []tea.WindowSizeMsg{{Width: 48, Height: 16}, {Width: 80, Height: 24}, {Width: 140, Height: 50}} {
			m.Update(size)
			view := m.View().Content
			if lipgloss.Height(view) != size.Height || lipgloss.Width(view) > size.Width || !strings.Contains(viewText(m), "Esc back") {
				t.Fatalf("monitor %q exceeds %dx%d:\n%s", name, size.Width, size.Height, viewText(m))
			}
		}
	}
}
