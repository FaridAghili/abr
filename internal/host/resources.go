package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"abr/internal/config"
	"abr/internal/resources"
)

// Shared services run as their own system users; root-owned masters are excluded.
var sharedUsageUsers = []struct{ name, user string }{{"Caddy", "caddy"}, {"MySQL", "mysql"}, {"Redis", "redis"}}

type UsageOptions struct {
	JSON     bool
	Interval time.Duration
}

// Usage prints live CPU and memory per app, measured over the interval.
func (h Host) Usage(name string, o UsageOptions) error {
	if err := h.guard(); err != nil {
		return err
	}
	if h.DryRun {
		cfg, err := config.Load(h.Manager.ConfigPath)
		if err != nil {
			return err
		}
		if _, err := selectedApps(cfg, name); err != nil {
			return err
		}
		h.say("Would read /proc for host CPU, memory, load and each app user's processes over %s", o.Interval)
		return nil
	}
	report, _, err := h.UsageReport(name, nil, o.Interval)
	if err != nil {
		return err
	}
	return resources.Write(h.Output, report, o.JSON)
}

// UsageReport measures since previous, or waits for interval when previous is nil.
// Callers that refresh pass the returned sample back for the next report.
func (h Host) UsageReport(name string, previous *resources.Sample, interval time.Duration) (resources.Report, resources.Sample, error) {
	if h.DryRun {
		return resources.Report{}, resources.Sample{}, fmt.Errorf("live resource usage reads /proc on the VPS; it is unavailable in preview mode")
	}
	if err := h.guard(); err != nil {
		return resources.Report{}, resources.Sample{}, err
	}
	cfg, err := config.Load(h.Manager.ConfigPath)
	if err != nil {
		return resources.Report{}, resources.Sample{}, err
	}
	apps, err := selectedApps(cfg, name)
	if err != nil {
		return resources.Report{}, resources.Sample{}, err
	}
	accounts, err := h.usageAccounts(apps, name == "")
	if err != nil {
		return resources.Report{}, resources.Sample{}, err
	}
	uids := map[uint32]bool{}
	for _, a := range accounts {
		if a.Known {
			uids[a.UID] = true
		}
	}
	if previous == nil {
		first, err := h.usageSample(uids)
		if err != nil {
			return resources.Report{}, resources.Sample{}, err
		}
		previous = &first
		time.Sleep(interval)
	}
	current, err := h.usageSample(uids)
	if err != nil {
		return resources.Report{}, resources.Sample{}, err
	}
	return resources.Build(accounts, *previous, current), current, nil
}

func (h Host) usageAccounts(apps []config.App, shared bool) ([]resources.Account, error) {
	data, err := os.ReadFile(h.path("/etc/passwd"))
	if err != nil {
		return nil, err
	}
	entries := map[string][]string{}
	for line := range strings.SplitSeq(string(data), "\n") {
		parts := strings.Split(line, ":")
		if len(parts) == 7 {
			entries[parts[0]] = parts
		}
	}
	uid := func(parts []string) (uint32, bool) {
		value, err := strconv.ParseUint(parts[2], 10, 32)
		return uint32(value), err == nil && value > 0
	}
	accounts := make([]resources.Account, 0, len(apps)+len(sharedUsageUsers))
	for _, a := range apps {
		account := resources.Account{Name: a.Name, Kind: "app", Type: a.Type}
		// Attribute processes only to the verified dedicated account, never a reused name.
		if parts, ok := entries[a.User]; ok && validRuntimeAccount(parts, a.User) && parts[4] == "abr-"+a.Name {
			account.UID, account.Known = uid(parts)
		}
		accounts = append(accounts, account)
	}
	if shared {
		for _, s := range sharedUsageUsers {
			if parts, ok := entries[s.user]; ok {
				if value, ok := uid(parts); ok {
					accounts = append(accounts, resources.Account{Name: s.name, Kind: "shared", UID: value, Known: true})
				}
			}
		}
	}
	return accounts, nil
}

func (h Host) usageSample(uids map[uint32]bool) (resources.Sample, error) {
	proc := h.path("/proc")
	s := resources.Sample{At: time.Now(), Processes: map[resources.ProcessKey]resources.Process{}}
	data, err := os.ReadFile(filepath.Join(proc, "stat"))
	if err != nil {
		return s, err
	}
	if s.CPUTotal, s.CPUIdle, s.Cores, err = resources.ParseCPU(data); err != nil {
		return s, err
	}
	if data, err = os.ReadFile(filepath.Join(proc, "meminfo")); err != nil {
		return s, err
	}
	if s.MemoryTotal, s.Available, s.SwapTotal, s.SwapFree, err = resources.ParseMemory(data); err != nil {
		return s, err
	}
	if data, err = os.ReadFile(filepath.Join(proc, "loadavg")); err != nil {
		return s, err
	}
	if s.Load, err = resources.ParseLoad(data); err != nil {
		return s, err
	}
	entries, err := os.ReadDir(proc)
	if err != nil {
		return s, err
	}
	page := uint64(os.Getpagesize())
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		dir := filepath.Join(proc, entry.Name())
		info, err := os.Stat(dir)
		if err != nil {
			continue // The process exited during the scan.
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !uids[stat.Uid] {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, "stat"))
		if err != nil {
			continue
		}
		ps, err := resources.ParseProcessStat(data)
		if err != nil {
			continue
		}
		p := resources.Process{UID: stat.Uid, Unit: "other", Ticks: ps.Ticks, Memory: ps.RSSPages * page, Threads: ps.Threads}
		if data, err := os.ReadFile(filepath.Join(dir, "smaps_rollup")); err == nil {
			if pss, ok := resources.ParsePSS(data); ok {
				p.Memory = pss
			}
		}
		if data, err := os.ReadFile(filepath.Join(dir, "cgroup")); err == nil {
			p.Unit = resources.ParseUnit(data)
		}
		s.Processes[resources.ProcessKey{PID: pid, Start: ps.Start}] = p
	}
	return s, nil
}
