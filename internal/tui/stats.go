package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"abr/internal/disk"
	"abr/internal/resources"
)

const statsRefresh = 2 * time.Second

var statsSorts = []string{"memory", "cpu", "name"}

// statsView is the live resource monitor; it only reads measurements.
type statsView struct {
	name         string // Empty shows all apps and shared services.
	report       resources.Report
	sample       *resources.Sample
	err          error
	loaded       bool
	sort, cursor int
}

type statsMsg struct {
	generation int
	report     resources.Report
	sample     resources.Sample
	err        error
}
type statsTick struct{ generation int }

func (m *model) statsMonitor(name string) tea.Cmd {
	m.back = m.menu
	if name != "" {
		// Return to the all-app monitor when opened from one of its rows.
		if m.page == "stats" && m.stats != nil && m.stats.name == "" {
			sort := m.stats.sort
			m.back = func() tea.Cmd {
				cmd := m.statsMonitor("")
				m.stats.sort = sort
				return cmd
			}
		}
	}
	m.generation++
	m.page, m.form, m.groups, m.next = "stats", nil, nil, nil
	m.notice, m.context = "", ""
	m.title = "Resource monitor"
	if name != "" {
		m.title = name + " / Resource usage"
	}
	m.stats = &statsView{name: name}
	return tea.Batch(m.measure(), m.spinner.Tick)
}

func (m *model) measure() tea.Cmd {
	generation, measure, name := m.generation, m.options.Usage, m.stats.name
	previous := m.stats.sample
	return func() tea.Msg {
		if measure == nil {
			return statsMsg{generation: generation, err: fmt.Errorf("resource monitor is unavailable")}
		}
		report, sample, err := measure(name, previous)
		return statsMsg{generation: generation, report: report, sample: sample, err: err}
	}
}

func (m *model) updateStats(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case statsMsg:
		if m.page != "stats" || msg.generation != m.generation {
			return nil, true // A monitor that was left keeps no timer running.
		}
		m.stats.loaded, m.stats.err = true, msg.err
		if msg.err == nil {
			m.stats.report, m.stats.sample = msg.report, &msg.sample
			m.stats.cursor = min(m.stats.cursor, max(0, len(msg.report.Apps)-1))
		}
		generation := m.generation
		return tea.Tick(statsRefresh, func(time.Time) tea.Msg { return statsTick{generation} }), true
	case statsTick:
		if m.page != "stats" || msg.generation != m.generation {
			return nil, true
		}
		return m.measure(), true
	case tea.KeyPressMsg:
		if m.page != "stats" {
			return nil, false
		}
		rows := m.statsRows()
		switch msg.String() {
		case "up", "ctrl+p", "ctrl+k":
			m.stats.cursor = max(0, m.stats.cursor-1)
		case "down", "ctrl+n", "ctrl+j":
			m.stats.cursor = min(max(0, len(rows)-1), m.stats.cursor+1)
		case "home":
			m.stats.cursor = 0
		case "end":
			m.stats.cursor = max(0, len(rows)-1)
		case "s":
			m.stats.sort = (m.stats.sort + 1) % len(statsSorts)
		case "enter":
			if m.stats.name == "" && m.stats.cursor < len(rows) && rows[m.stats.cursor].Kind == "app" {
				return m.statsMonitor(rows[m.stats.cursor].Name), true
			}
			if m.stats.name != "" {
				return m.goBack(), true
			}
		default:
			return nil, false
		}
		return nil, true
	}
	return nil, false
}

// statsRows orders apps first, then shared services, each by the chosen measure.
func (m *model) statsRows() []resources.App {
	if m.stats == nil {
		return nil
	}
	rows := slices.Clone(m.stats.report.Apps)
	by := statsSorts[m.stats.sort]
	slices.SortStableFunc(rows, func(a, b resources.App) int {
		if a.Kind != b.Kind {
			return strings.Compare(a.Kind, b.Kind) // "app" before "shared".
		}
		switch by {
		case "cpu":
			if a.CPUPercent != b.CPUPercent {
				if a.CPUPercent > b.CPUPercent {
					return -1
				}
				return 1
			}
		case "memory":
			if a.MemoryBytes != b.MemoryBytes {
				if a.MemoryBytes > b.MemoryBytes {
					return -1
				}
				return 1
			}
		}
		return strings.Compare(a.Name, b.Name)
	})
	return rows
}

func (m *model) statsFooter() string {
	if m.stats != nil && m.stats.name != "" {
		return "s sort · Enter / Esc back"
	}
	return "↑/↓ select · Enter services\ns sort · Esc back"
}

// Thresholds: CPU share of all cores, memory share of RAM.
const cpuLight, cpuHeavy, memoryLight, memoryHeavy = 25, 60, 15, 35

func (m *model) statsBody(height int) string {
	p := colors(m.dark)
	width := m.bodyWidth()
	s := m.stats
	if !s.loaded {
		return m.spinner.View() + " Measuring CPU and memory…"
	}
	if s.err != nil && s.sample == nil {
		return p.danger.Render("✗ Cannot read resource usage") + "\n\n" + ansi.Wrap(clean(s.err.Error()), width, "")
	}
	r := s.report
	status := p.success.Render("● Live") + p.muted.Render(fmt.Sprintf(" · updated %s · every %s · sort: %s", r.MeasuredAt.Local().Format("15:04:05"), statsRefresh, statsSorts[s.sort]))
	if s.err != nil {
		status = p.warning.Render("● Stale") + p.muted.Render(" · "+clean(s.err.Error()))
	}
	lines := []string{ansi.Truncate(status, width, "…"), ""}

	// Host gauges.
	gauge := max(6, min(24, width-40))
	h := r.Host
	lines = append(lines,
		fmt.Sprintf("%s %s %s %s", p.text.Render("CPU   "), p.bar(h.CPUPercent, gauge, cpuLight, cpuHeavy), p.level(h.CPUPercent, cpuLight, cpuHeavy).Render(fmt.Sprintf("%6s", resources.Percent(h.CPUPercent))), p.muted.Render(fmt.Sprintf("%d cores · load %.2f %.2f %.2f", h.Cores, h.Load[0], h.Load[1], h.Load[2]))),
		fmt.Sprintf("%s %s %s %s", p.text.Render("Memory"), p.bar(h.MemoryPercent, gauge, 60, 85), p.level(h.MemoryPercent, 60, 85).Render(fmt.Sprintf("%6s", resources.Percent(h.MemoryPercent))), p.muted.Render(disk.Bytes(h.MemoryUsed)+" / "+disk.Bytes(h.MemoryTotal))))
	if h.SwapTotal > 0 {
		swap := float64(h.SwapUsed) * 100 / float64(h.SwapTotal)
		lines = append(lines, fmt.Sprintf("%s %s %s %s", p.text.Render("Swap  "), p.bar(swap, gauge, 25, 60), p.level(swap, 25, 60).Render(fmt.Sprintf("%6s", resources.Percent(swap))), p.muted.Render(disk.Bytes(h.SwapUsed)+" / "+disk.Bytes(h.SwapTotal))))
	}
	lines = append(lines, "")

	if s.name != "" {
		lines = append(lines, m.serviceLines(p, width)...)
	} else {
		lines = append(lines, m.appLines(p, width, height-len(lines))...)
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], width, "…")
	}
	return strings.Join(lines, "\n")
}

type statsColumns struct {
	name, bar int
	processes bool
}

func columns(width, longest int) statsColumns {
	name := max(8, min(20, longest))
	// Fixed text is 26 columns, plus 6 for processes when there is room.
	c := statsColumns{name: name, processes: width-name-32 >= 6}
	rest := width - name - 26
	if c.processes {
		rest -= 6
	}
	c.bar = max(3, min(14, rest/2))
	return c
}

// Row layout: marker 2, name, space, CPU bar+7, space, memory bar+10+5, then space and processes 5.
func (p palette) usageRow(c statsColumns, marker, name string, cpu float64, memory uint64, memoryPercent float64, processes int) string {
	row := marker + p.text.Render(pad(ansi.Truncate(name, c.name, "…"), c.name)) + " " +
		p.bar(cpu, c.bar, cpuLight, cpuHeavy) + p.level(cpu, cpuLight, cpuHeavy).Render(fmt.Sprintf("%7s", resources.Percent(cpu))) + " " +
		p.bar(memoryPercent, c.bar, memoryLight, memoryHeavy) + p.level(memoryPercent, memoryLight, memoryHeavy).Render(fmt.Sprintf("%10s", disk.Bytes(memory))) + p.muted.Render(fmt.Sprintf("%4.0f%%", memoryPercent))
	if c.processes {
		row += " " + p.muted.Render(fmt.Sprintf("%5d", processes))
	}
	return row
}

func (p palette) usageHeader(c statsColumns, label string) string {
	header := pad(label, c.name) + " " + pad("CPU", c.bar+7) + " " + "MEMORY"
	if c.processes {
		header = pad(header, c.name+c.bar*2+24) + " PROCS"
	}
	return "  " + p.muted.Render(header)
}

func pad(s string, width int) string {
	return s + strings.Repeat(" ", max(0, width-ansi.StringWidth(s)))
}

func (m *model) appLines(p palette, width, height int) []string {
	rows := m.statsRows()
	if len(rows) == 0 {
		return []string{p.muted.Render("No apps registered. Choose Add application from the main menu.")}
	}
	longest := 0
	for _, row := range rows {
		longest = max(longest, len(row.Name))
	}
	c := columns(width, longest)
	var heaviest string
	var most uint64
	for _, row := range rows {
		if row.Kind == "app" && row.MemoryBytes > most {
			heaviest, most = row.Name, row.MemoryBytes
		}
	}
	type line struct {
		text string
		row  int // -1 for headings
	}
	var all []line
	kind := ""
	for i, row := range rows {
		if row.Kind != kind {
			kind = row.Kind
			label := "APPS"
			if kind == "shared" {
				label = "SHARED"
				all = append(all, line{"", -1})
			}
			all = append(all, line{p.usageHeader(c, label), -1})
		}
		marker := "  "
		if i == m.stats.cursor {
			marker = p.key.Render("▸ ")
		}
		switch {
		case !row.Known:
			all = append(all, line{marker + p.text.Render(pad(ansi.Truncate(row.Name, c.name, "…"), c.name)) + " " + p.muted.Render("Ubuntu user not created yet"), i})
		case row.Processes == 0:
			all = append(all, line{marker + p.text.Render(pad(ansi.Truncate(row.Name, c.name, "…"), c.name)) + " " + p.muted.Render("idle · no running processes"), i})
		default:
			text := p.usageRow(c, marker, row.Name, row.CPUPercent, row.MemoryBytes, row.MemoryPercent, row.Processes)
			if row.Name == heaviest && row.Kind == "app" && len(rows) > 1 {
				text += p.warning.Render(" ▲")
			}
			all = append(all, line{text, i})
		}
	}
	// Keep the selected row visible in short terminals.
	height = max(1, height)
	start := 0
	for i, l := range all {
		if l.row == m.stats.cursor && i >= start+height {
			start = i - height + 1
		}
	}
	end := min(len(all), start+height)
	out := make([]string, 0, end-start)
	for _, l := range all[start:end] {
		out = append(out, l.text)
	}
	return out
}

func (m *model) serviceLines(p palette, width int) []string {
	if len(m.stats.report.Apps) == 0 {
		return nil
	}
	app := m.stats.report.Apps[0]
	if !app.Known {
		return []string{p.muted.Render("The Ubuntu user is not created yet. Register the app on this VPS first.")}
	}
	if len(app.Services) == 0 {
		return []string{p.muted.Render("Idle · no running processes. PHP-FPM workers start on the next request.")}
	}
	services := slices.Clone(app.Services)
	switch statsSorts[m.stats.sort] {
	case "cpu":
		slices.SortStableFunc(services, func(a, b resources.Service) int {
			if a.CPUPercent > b.CPUPercent {
				return -1
			} else if a.CPUPercent < b.CPUPercent {
				return 1
			}
			return 0
		})
	case "name":
		slices.SortStableFunc(services, func(a, b resources.Service) int { return strings.Compare(a.Name, b.Name) })
	}
	longest := 0
	names := make([]string, len(services))
	for i, s := range services {
		names[i] = serviceLabel(app.Name, s.Name)
		longest = max(longest, len(names[i]))
	}
	c := columns(width, longest)
	lines := []string{p.usageRow(c, p.accent.Render("Σ "), "total", app.CPUPercent, app.MemoryBytes, app.MemoryPercent, app.Processes), "", p.usageHeader(c, "SERVICE")}
	for i, s := range services {
		lines = append(lines, p.usageRow(c, "  ", names[i], s.CPUPercent, s.MemoryBytes, s.MemoryPercent, s.Processes))
	}
	return lines
}

func serviceLabel(app, unit string) string {
	if name, ok := strings.CutPrefix(unit, "abr-"+app+"-"); ok {
		return name
	}
	if strings.HasPrefix(unit, "php") && strings.HasSuffix(unit, "-fpm") {
		return "web (PHP-FPM)"
	}
	if unit == "other" {
		return "other (shells, jobs)"
	}
	return unit
}
