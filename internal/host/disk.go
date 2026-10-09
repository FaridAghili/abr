package host

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"abr/internal/config"
	"abr/internal/disk"
	"abr/internal/storage"
	"golang.org/x/sys/unix"
)

const diskCacheTTL = time.Minute

type DiskOptions struct {
	All, Refresh, JSON, FilesystemOnly bool
}

type diskSource struct {
	App                   config.App
	Project, Home, Deploy string
	Database              bool
}

type diskCache struct {
	Signature string      `json:"signature"`
	Report    disk.Report `json:"report"`
}

func (h Host) DiskUsage(name string, o DiskOptions) error {
	if name != "" && (o.All || o.FilesystemOnly) {
		return fmt.Errorf("use abr disk [APP], --all, or --filesystem-only")
	}
	if err := h.guard(); err != nil {
		return err
	}
	if h.DryRun {
		if o.JSON {
			return fmt.Errorf("--json requires actual disk measurements; omit --dry-run on the VPS")
		}
		if o.FilesystemOnly {
			h.say("Would read live filesystem capacity, used and available space; no app scan")
			return nil
		}
		cfg, err := config.Load(h.Manager.ConfigPath)
		if err != nil {
			return err
		}
		apps, err := selectedApps(cfg, name)
		if err != nil {
			return err
		}
		for _, a := range apps {
			h.say("Would measure %s: %s, recorded runtime home, deployment logs/history and managed database metadata", a.Name, a.Directory)
		}
		h.say("Would read live filesystem capacity, used and available space; app scans are cached for one minute")
		return nil
	}
	report := disk.Report{Apps: []disk.App{}, FilesystemOnly: o.FilesystemOnly}
	filesystemPaths := []string{h.path("/"), h.path(h.AppsDir), h.path(h.Manager.StateDir), h.path("/var/lib/abr-users"), h.path("/var/lib/mysql")}
	if !o.FilesystemOnly {
		cfg, err := config.Load(h.Manager.ConfigPath)
		if err != nil {
			return err
		}
		apps, err := selectedApps(cfg, name)
		if err != nil {
			return err
		}
		sources, err := h.diskSources(apps)
		if err != nil {
			return err
		}
		for _, s := range sources {
			filesystemPaths = append(filesystemPaths, s.Project, s.Deploy)
			if s.Home != "" {
				filesystemPaths = append(filesystemPaths, s.Home)
			}
		}
		report, err = h.cachedDiskUsage(name, sources, o.Refresh)
		if err != nil {
			return err
		}
		report.Notes = []string{
			"File sizes use allocated blocks. Symlink targets and nested filesystems are excluded; hard links count once per scan.",
			"~ includes MySQL data/index metadata estimates. Shared runtimes, journals, MySQL shared files and external backups are excluded.",
		}
	}
	var err error
	report.Filesystems, err = diskFilesystems(filesystemPaths)
	if err != nil {
		return err
	}
	report.FilesystemMeasuredAt = time.Now().UTC()
	report.Notes = append(report.Notes, "Filesystem space is live; AVAILABLE excludes filesystem-reserved blocks.")
	return disk.Write(h.Output, report, o.JSON)
}

func selectedApps(cfg config.Config, name string) ([]config.App, error) {
	apps := slices.Clone(cfg.Apps)
	if name != "" {
		index := slices.IndexFunc(apps, func(a config.App) bool { return a.Name == name })
		if index < 0 {
			return nil, fmt.Errorf("unknown application %q", name)
		}
		apps = apps[index : index+1]
	}
	slices.SortFunc(apps, func(a, b config.App) int { return strings.Compare(a.Name, b.Name) })
	return apps, nil
}

func (h Host) diskSources(apps []config.App) ([]diskSource, error) {
	sources := make([]diskSource, 0, len(apps))
	for _, a := range apps {
		s := diskSource{App: a, Project: h.path(a.Directory), Deploy: h.path(filepath.Join(h.Manager.StateDir, "deployments", a.Name))}
		data, err := h.read(h.userPath(a))
		if err == nil {
			var record userRecord
			if json.Unmarshal(data, &record) != nil || record.App != a.Name || record.User != a.User || record.Home != "/var/lib/abr-users/"+a.User {
				return nil, fmt.Errorf("%s: invalid runtime home ownership record", a.Name)
			}
			s.Home = h.path(record.Home)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		data, err = h.read(h.credentialsPath(a.Name))
		if err == nil {
			var record credentials
			if json.Unmarshal(data, &record) != nil || record.App != a.Name || record.Database != databaseName(a.Name) || record.User != databaseUser(a.Name) {
				return nil, fmt.Errorf("%s: invalid managed database ownership record", a.Name)
			}
			s.Database = true // Include retained databases even when disabled in config.
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		sources = append(sources, s)
	}
	return sources, nil
}

func (h Host) cachedDiskUsage(name string, sources []diskSource, refresh bool) (disk.Report, error) {
	directory := h.path(filepath.Join(h.Manager.StateDir, "disk-usage"))
	if err := h.trustedAncestor(directory); err != nil {
		return disk.Report{}, err
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return disk.Report{}, err
	}
	if err := h.trustedDirectory(directory); err != nil {
		return disk.Report{}, err
	}
	key := "all"
	if name != "" {
		key = "app-" + name
	}
	path := filepath.Join(directory, key+".json")
	lockPath := filepath.Join(directory, key+".lock")
	for _, p := range []string{path, lockPath} {
		if err := h.trustedFile(p); err != nil && !os.IsNotExist(err) {
			return disk.Report{}, err
		}
	}
	encoded, err := json.Marshal(sources)
	if err != nil {
		return disk.Report{}, err
	}
	signature := fmt.Sprintf("%x", sha256.Sum256(encoded))
	var report disk.Report
	err = storage.WithLock(lockPath, func() error {
		if !refresh {
			data, err := readProjectFile(path, 1<<20)
			if err == nil {
				var cache diskCache
				if json.Unmarshal(data, &cache) == nil && cache.Signature == signature && !cache.Report.MeasuredAt.IsZero() {
					age := time.Since(cache.Report.MeasuredAt)
					if age >= 0 && age < diskCacheTTL {
						report = cache.Report
						report.Cached = true
						return nil
					}
				}
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		var err error
		report, err = h.measureDiskUsage(sources)
		if err != nil {
			return err // Do not serve stale results or cache a failed measurement.
		}
		data, err := json.Marshal(diskCache{Signature: signature, Report: report})
		if err != nil {
			return err
		}
		return storage.AtomicWrite(path, data)
	})
	return report, err
}

func (h Host) diskCommand(c Command) ([]byte, error) {
	runner := h.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	c.Private = true // Keep command output and SQL out of text/JSON reports.
	return runner.Run(c)
}

func (h Host) measureDiskUsage(sources []diskSource) (disk.Report, error) {
	report := disk.Report{Apps: make([]disk.App, 0, len(sources))}
	var paths []string
	for _, s := range sources {
		for _, path := range []string{s.Project, s.Home, s.Deploy} {
			if path == "" {
				continue
			}
			info, err := os.Lstat(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return report, err
			}
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil || !info.IsDir() || resolved != filepath.Clean(path) {
				return report, fmt.Errorf("disk usage path must be a directory without symlink ancestors: %s", path)
			}
			paths = append(paths, path)
		}
	}
	sizes := map[string]uint64{}
	if len(paths) > 0 {
		output, err := h.diskCommand(Command{Name: "du", Args: []string{"--summarize", "--block-size=1", "--one-file-system", "--null", "--files0-from=-"}, Input: []byte(strings.Join(paths, "\x00") + "\x00")})
		if err != nil {
			return report, fmt.Errorf("measure app files: %w", err)
		}
		sizes, err = disk.ParseDU(output, paths)
		if err != nil {
			return report, err
		}
	}
	databases, err := h.diskDatabases(sources)
	if err != nil {
		return report, err
	}
	for _, s := range sources {
		a := disk.App{Name: s.App.Name, ProjectBytes: sizes[s.Project], HomeBytes: sizes[s.Home], DeploymentBytes: sizes[s.Deploy], DatabaseManaged: s.Database, DatabaseBytes: databases[databaseName(s.App.Name)]}
		a.FilesBytes = a.ProjectBytes + a.HomeBytes + a.DeploymentBytes
		a.TotalBytes = a.FilesBytes + a.DatabaseBytes
		report.FilesBytes += a.FilesBytes
		report.DatabaseBytes += a.DatabaseBytes
		report.TotalBytes += a.TotalBytes
		report.Apps = append(report.Apps, a)
	}
	report.MeasuredAt = time.Now().UTC()
	return report, nil
}

func (h Host) diskDatabases(sources []diskSource) (map[string]uint64, error) {
	selected := map[string]bool{}
	var names []string
	for _, s := range sources {
		if s.Database {
			name := databaseName(s.App.Name) // Validated app names cannot contain SQL quotes.
			selected[name] = true
			names = append(names, "'"+name+"'")
		}
	}
	sizes := map[string]uint64{}
	if len(names) == 0 {
		return sizes, nil
	}
	sql := "SELECT s.SCHEMA_NAME, COALESCE(SUM(t.DATA_LENGTH + t.INDEX_LENGTH), 0) FROM information_schema.SCHEMATA s LEFT JOIN information_schema.TABLES t ON t.TABLE_SCHEMA = s.SCHEMA_NAME WHERE s.SCHEMA_NAME IN (" + strings.Join(names, ",") + ") GROUP BY s.SCHEMA_NAME;\n"
	output, err := h.diskCommand(Command{Name: "mysql", Args: []string{"--protocol=socket", "--user=root", "--batch", "--skip-column-names"}, Input: []byte(sql)})
	if err != nil {
		return nil, fmt.Errorf("measure managed database metadata: %w", err)
	}
	for _, row := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if row == "" {
			continue
		}
		name, value, ok := strings.Cut(row, "\t")
		if !ok || !selected[name] {
			return nil, fmt.Errorf("unexpected database size output")
		}
		if _, duplicate := sizes[name]; duplicate {
			return nil, fmt.Errorf("duplicate database size output")
		}
		size, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid database size output")
		}
		sizes[name] = size
	}
	return sizes, nil
}

// statfs is constant-time; even cached app reports always get fresh free space.
func diskFilesystems(paths []string) ([]disk.Filesystem, error) {
	result := make([]disk.Filesystem, 0)
	seen := map[uint64]bool{}
	for _, path := range paths {
		for {
			_, err := os.Stat(path)
			if !os.IsNotExist(err) || filepath.Dir(path) == path {
				break
			}
			path = filepath.Dir(path)
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		device := uint64(info.Sys().(*syscall.Stat_t).Dev)
		if seen[device] {
			continue
		}
		var stat unix.Statfs_t
		if err := unix.Statfs(path, &stat); err != nil {
			return nil, err
		}
		seen[device] = true
		block := uint64(stat.Bsize)
		available := uint64(stat.Bavail)
		if available > stat.Blocks {
			available = 0 // Some filesystems encode negative availability as unsigned.
		}
		f := disk.Filesystem{Path: path, CapacityBytes: stat.Blocks * block, UsedBytes: (stat.Blocks - stat.Bfree) * block, AvailableBytes: available * block}
		if f.CapacityBytes > 0 {
			f.UsedPercent = float64(f.UsedBytes) / float64(f.CapacityBytes) * 100
		}
		result = append(result, f)
	}
	return result, nil
}
