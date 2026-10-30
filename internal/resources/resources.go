// Package resources computes and formats portable CPU and memory reports from
// Linux /proc text. Host sampling lives in host.
package resources

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"time"

	"abr/internal/disk"
	"abr/internal/table"
)

// Process is one measured process owned by a watched account.
type Process struct {
	UID     uint32
	Unit    string // systemd unit or "other"
	Ticks   uint64 // user + system CPU time
	Memory  uint64 // PSS bytes when readable, otherwise RSS
	Threads int
}

// ProcessKey separates a reused PID from the process it replaced.
type ProcessKey struct {
	PID   int
	Start uint64
}

// Sample is one point-in-time reading of the host and watched processes.
type Sample struct {
	At                     time.Time
	CPUTotal, CPUIdle      uint64
	Cores                  int
	MemoryTotal, Available uint64
	SwapTotal, SwapFree    uint64
	Load                   [3]float64
	Processes              map[ProcessKey]Process
}

// Account maps a dedicated app user or shared service user to a report row.
type Account struct {
	Name, Kind, Type string // Kind is "app" or "shared".
	UID              uint32
	Known            bool // False when the runtime user is not created yet.
}

type Service struct {
	Name          string  `json:"name"`
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryBytes   uint64  `json:"memory_bytes"`
	MemoryPercent float64 `json:"memory_percent"`
	Processes     int     `json:"processes"`
}

type App struct {
	Name          string    `json:"name"`
	Kind          string    `json:"kind"`
	Type          string    `json:"type,omitempty"`
	Known         bool      `json:"known"`
	CPUPercent    float64   `json:"cpu_percent"`
	MemoryBytes   uint64    `json:"memory_bytes"`
	MemoryPercent float64   `json:"memory_percent"`
	Processes     int       `json:"processes"`
	Threads       int       `json:"threads"`
	Services      []Service `json:"services"`
}

type Host struct {
	Cores         int        `json:"cores"`
	CPUPercent    float64    `json:"cpu_percent"`
	MemoryTotal   uint64     `json:"memory_total_bytes"`
	MemoryUsed    uint64     `json:"memory_used_bytes"`
	MemoryPercent float64    `json:"memory_percent"`
	SwapTotal     uint64     `json:"swap_total_bytes"`
	SwapUsed      uint64     `json:"swap_used_bytes"`
	Load          [3]float64 `json:"load_average"`
}

type Report struct {
	MeasuredAt time.Time     `json:"measured_at"`
	Interval   time.Duration `json:"interval_ns"`
	Host       Host          `json:"host"`
	Apps       []App         `json:"apps"`
	Notes      []string      `json:"notes"`
}

// Notes describe how measurements are attributed.
var Notes = []string{
	"CPU is the share of all cores over the interval; memory is proportional (PSS), so shared pages are split between processes.",
	"App rows count every process of the app's dedicated user, including PHP-FPM pool workers. Shared rows count service users.",
}

// Build compares two samples. CPU is zero when previous is empty.
func Build(accounts []Account, previous, current Sample) Report {
	r := Report{MeasuredAt: current.At, Apps: []App{}, Notes: Notes}
	if !previous.At.IsZero() {
		r.Interval = current.At.Sub(previous.At)
	}
	total := delta(current.CPUTotal, previous.CPUTotal)
	if previous.At.IsZero() {
		total = 0
	}
	r.Host = Host{Cores: current.Cores, MemoryTotal: current.MemoryTotal, SwapTotal: current.SwapTotal, Load: current.Load}
	if total > 0 {
		r.Host.CPUPercent = percent(total-min(total, delta(current.CPUIdle, previous.CPUIdle)), total)
	}
	if current.MemoryTotal > current.Available {
		r.Host.MemoryUsed = current.MemoryTotal - current.Available
	}
	r.Host.MemoryPercent = percent(r.Host.MemoryUsed, current.MemoryTotal)
	if current.SwapTotal > current.SwapFree {
		r.Host.SwapUsed = current.SwapTotal - current.SwapFree
	}
	for _, account := range accounts {
		app := App{Name: account.Name, Kind: account.Kind, Type: account.Type, Known: account.Known, Services: []Service{}}
		services := map[string]*Service{}
		for key, p := range current.Processes {
			if !account.Known || p.UID != account.UID {
				continue
			}
			ticks := p.Ticks
			if before, ok := previous.Processes[key]; ok {
				ticks = delta(p.Ticks, before.Ticks)
			}
			cpu := 0.0
			if total > 0 {
				// /proc/stat totals sum every core, so this is a share of the host.
				cpu = percent(ticks, total)
			}
			app.CPUPercent += cpu
			app.MemoryBytes += p.Memory
			app.Processes++
			app.Threads += p.Threads
			s := services[p.Unit]
			if s == nil {
				s = &Service{Name: p.Unit}
				services[p.Unit] = s
			}
			s.CPUPercent += cpu
			s.MemoryBytes += p.Memory
			s.Processes++
		}
		app.MemoryPercent = percent(app.MemoryBytes, current.MemoryTotal)
		for _, s := range services {
			s.MemoryPercent = percent(s.MemoryBytes, current.MemoryTotal)
			app.Services = append(app.Services, *s)
		}
		slices.SortFunc(app.Services, func(a, b Service) int {
			if a.MemoryBytes != b.MemoryBytes {
				return compare(b.MemoryBytes, a.MemoryBytes)
			}
			return strings.Compare(a.Name, b.Name)
		})
		r.Apps = append(r.Apps, app)
	}
	return r
}

func delta(current, previous uint64) uint64 {
	if current < previous {
		return 0
	}
	return current - previous
}

func percent(part, whole uint64) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) * 100 / float64(whole)
}

func compare(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// ParseCPU reads the aggregate line and core count from /proc/stat.
func ParseCPU(data []byte) (total, idle uint64, cores int, err error) {
	found := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		if fields[0] != "cpu" {
			cores++
			continue
		}
		if len(fields) < 5 {
			return 0, 0, 0, fmt.Errorf("invalid /proc/stat cpu line")
		}
		// user nice system idle iowait irq softirq steal; guest time is already in user.
		for i, field := range fields[1:min(len(fields), 9)] {
			value, parseErr := strconv.ParseUint(field, 10, 64)
			if parseErr != nil {
				return 0, 0, 0, fmt.Errorf("invalid /proc/stat cpu value")
			}
			total += value
			if i == 3 || i == 4 {
				idle += value
			}
		}
		found = true
	}
	if !found {
		return 0, 0, 0, fmt.Errorf("/proc/stat has no cpu line")
	}
	return total, idle, max(cores, 1), nil
}

// ParseMemory reads total, available and swap bytes from /proc/meminfo.
func ParseMemory(data []byte) (total, available, swapTotal, swapFree uint64, err error) {
	values := map[string]uint64{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		name, rest, ok := strings.Cut(scanner.Text(), ":")
		fields := strings.Fields(rest)
		if !ok || len(fields) == 0 {
			continue
		}
		value, parseErr := strconv.ParseUint(fields[0], 10, 64)
		if parseErr != nil {
			continue
		}
		values[name] = value * 1024
	}
	total, ok := values["MemTotal"]
	if !ok || total == 0 {
		return 0, 0, 0, 0, fmt.Errorf("/proc/meminfo has no MemTotal")
	}
	available, ok = values["MemAvailable"]
	if !ok {
		available = values["MemFree"] + values["Buffers"] + values["Cached"]
	}
	return total, min(available, total), values["SwapTotal"], values["SwapFree"], nil
}

// ParseLoad reads the 1, 5 and 15 minute load averages from /proc/loadavg.
func ParseLoad(data []byte) ([3]float64, error) {
	var load [3]float64
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return load, fmt.Errorf("invalid /proc/loadavg")
	}
	for i := range load {
		value, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return load, fmt.Errorf("invalid /proc/loadavg")
		}
		load[i] = value
	}
	return load, nil
}

// ProcessStat holds the /proc/PID/stat fields used by reports.
type ProcessStat struct {
	Ticks, Start, RSSPages uint64
	Threads                int
}

// ParseProcessStat reads /proc/PID/stat; the command name may contain spaces or parentheses.
func ParseProcessStat(data []byte) (ProcessStat, error) {
	end := bytes.LastIndexByte(data, ')')
	if end < 0 {
		return ProcessStat{}, fmt.Errorf("invalid process stat")
	}
	// Fields after the name start at field 3 (state).
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 22 {
		return ProcessStat{}, fmt.Errorf("invalid process stat")
	}
	number := func(field int) (uint64, error) { return strconv.ParseUint(fields[field-3], 10, 64) }
	var s ProcessStat
	utime, err1 := number(14)
	stime, err2 := number(15)
	threads, err3 := number(20)
	start, err4 := number(22)
	rss, err5 := strconv.ParseInt(fields[24-3], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil {
		return ProcessStat{}, fmt.Errorf("invalid process stat")
	}
	s.Ticks, s.Threads, s.Start, s.RSSPages = utime+stime, int(threads), start, uint64(max(rss, 0))
	return s, nil
}

// ParsePSS reads proportional memory from /proc/PID/smaps_rollup.
func ParsePSS(data []byte) (uint64, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		name, rest, ok := strings.Cut(scanner.Text(), ":")
		fields := strings.Fields(rest)
		if ok && name == "Pss" && len(fields) > 0 {
			value, err := strconv.ParseUint(fields[0], 10, 64)
			return value * 1024, err == nil
		}
	}
	return 0, false
}

// ParseUnit returns the systemd service owning a process from /proc/PID/cgroup.
func ParseUnit(data []byte) string {
	for line := range strings.SplitSeq(string(data), "\n") {
		// The cgroup v2 entry is "0::/system.slice/abr-example-queue@1.service".
		path, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		for part := range strings.SplitSeq(path, "/") {
			if name, ok := strings.CutSuffix(part, ".service"); ok && name != "" && !strings.HasPrefix(part, "user@") {
				return name
			}
		}
	}
	return "other"
}

// Percent formats a share with one decimal place.
func Percent(value float64) string { return strconv.FormatFloat(value, 'f', 1, 64) + "%" }

func Write(w io.Writer, r Report, jsonOutput bool) error {
	if w == nil {
		w = io.Discard
	}
	if jsonOutput {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(r)
	}
	h := r.Host
	fmt.Fprintf(w, "Measured %s over %s\n", r.MeasuredAt.Local().Format(time.RFC3339), r.Interval.Round(10*time.Millisecond))
	fmt.Fprintf(w, "Host CPU %s of %d cores · Load %.2f %.2f %.2f\n", Percent(h.CPUPercent), h.Cores, h.Load[0], h.Load[1], h.Load[2])
	fmt.Fprintf(w, "Memory %s / %s (%s) · Swap %s / %s\n\n", disk.Bytes(h.MemoryUsed), disk.Bytes(h.MemoryTotal), Percent(h.MemoryPercent), disk.Bytes(h.SwapUsed), disk.Bytes(h.SwapTotal))
	apps := table.Table{Headers: []string{"NAME", "KIND", "CPU", "MEMORY", "MEM%", "PROCESSES"}, Right: []int{2, 3, 4, 5}}
	for _, a := range r.Apps {
		name := table.Styled(a.Name, table.Strong)
		if !a.Known {
			absent := table.Styled("—", table.Muted)
			apps.Rows = append(apps.Rows, []table.Cell{name, table.Text(a.Kind), absent, absent, absent, table.Styled("user not created", table.Muted)})
			continue
		}
		apps.Rows = append(apps.Rows, []table.Cell{name, table.Text(a.Kind), load(a.CPUPercent), table.Text(disk.Bytes(a.MemoryBytes)), load(a.MemoryPercent), table.Text(strconv.Itoa(a.Processes))})
	}
	if err := table.Write(w, apps); err != nil {
		return err
	}
	if len(r.Apps) == 1 && len(r.Apps[0].Services) > 0 {
		services := table.Table{Headers: []string{"SERVICE", "CPU", "MEMORY", "PROCESSES"}, Right: []int{1, 2, 3}}
		for _, s := range r.Apps[0].Services {
			services.Rows = append(services.Rows, []table.Cell{table.Styled(s.Name, table.Strong), load(s.CPUPercent), table.Text(disk.Bytes(s.MemoryBytes)), table.Text(strconv.Itoa(s.Processes))})
		}
		fmt.Fprintln(w)
		if err := table.Write(w, services); err != nil {
			return err
		}
	}
	fmt.Fprintln(w)
	for _, note := range r.Notes {
		fmt.Fprintln(w, note)
	}
	return nil
}

// load colors a share by how heavy it is.
func load(percent float64) table.Cell {
	cell := table.Styled(Percent(percent), table.Good)
	if percent >= 80 {
		cell.Tone = table.Bad
	} else if percent >= 50 {
		cell.Tone = table.Warn
	}
	return cell
}
