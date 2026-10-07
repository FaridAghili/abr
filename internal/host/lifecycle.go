package host

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"abr/internal/config"
	"abr/internal/ports"
	"abr/internal/services"
)

type manifest struct {
	Version      int `json:"version"`
	App, User    string
	Files, Units []string
	FPM, Enabled bool
}

func (h Host) manifestPath(name string) string {
	return filepath.Join(h.Manager.StateDir, "apps", name+".json")
}

func (h Host) application(name string) (config.App, ports.Registry, error) {
	var c config.Config
	var r ports.Registry
	var err error
	if h.DryRun {
		c, err = config.Load(h.Manager.ConfigPath)
		if err == nil {
			r, err = ports.Load(h.Manager.RegistryPath())
		}
	} else {
		c, r, err = h.Manager.Snapshot()
	}
	if err != nil {
		return config.App{}, r, err
	}
	for _, a := range c.Apps {
		if a.Name == name {
			return a, r, nil
		}
	}
	return config.App{}, r, fmt.Errorf("unknown application %q", name)
}

func unitAllowed(name, unit string) bool {
	return regexp.MustCompile(`^abr-` + regexp.QuoteMeta(name) + `-((octane|nuxt|queue@[1-9][0-9]*|nightwatch|inertia-ssr|scheduler)\.service|scheduler\.timer)$`).MatchString(unit)
}

func (h Host) fileAllowed(a config.App, path string) bool {
	if path != filepath.Clean(path) {
		return false
	}
	if path == filepath.Join(h.Manager.StateDir, "env", a.Name+".env") || path == "/etc/caddy/abr.d/abr-"+a.Name+".caddy" || path == "/etc/php/"+services.PHPVersion+"/fpm/pool.d/abr-"+a.Name+".conf" {
		return true
	}
	return filepath.Dir(path) == "/etc/systemd/system" && (unitAllowed(a.Name, filepath.Base(path)) || filepath.Base(path) == "abr-"+a.Name+"-queue@.service")
}

func (h Host) loadManifest(a config.App) (manifest, bool, error) {
	data, err := h.read(h.manifestPath(a.Name))
	if os.IsNotExist(err) {
		return manifest{Version: 1, App: a.Name, User: a.User}, false, nil
	}
	if err != nil {
		return manifest{}, false, err
	}
	var m manifest
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, false, fmt.Errorf("corrupt host manifest: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return m, false, fmt.Errorf("host manifest has trailing JSON")
	}
	if m.Version != 1 || m.App != a.Name || m.User != a.User {
		return m, false, fmt.Errorf("invalid host manifest identity; runtime user changes require removal/re-registration")
	}
	for _, path := range m.Files {
		if !h.fileAllowed(a, path) {
			return m, false, fmt.Errorf("manifest contains unmanaged path %s", path)
		}
	}
	for _, unit := range m.Units {
		if !unitAllowed(a.Name, unit) {
			return m, false, fmt.Errorf("manifest contains unmanaged unit %s", unit)
		}
	}
	return m, true, nil
}

func (h Host) saveManifest(m manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return h.write(h.manifestPath(m.App), data, 0600)
}

func managed(data []byte) bool {
	return bytes.HasPrefix(data, []byte(services.Marker)) || bytes.HasPrefix(data, []byte(strings.Replace(services.Marker, "#", ";", 1)))
}

func (h Host) checkFile(path string) error {
	data, err := h.read(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	info, err := os.Lstat(h.path(path))
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !managed(data) {
		return fmt.Errorf("refusing to replace/delete unmanaged file %s", path)
	}
	return nil
}

func (h Host) Enable(name string) error {
	return h.locked(func() error {
		a, r, err := h.application(name)
		if err != nil {
			return err
		}
		if !h.DryRun {
			r, err = h.Manager.Allocate()
			if err != nil {
				return err
			}
		}
		return h.enable(a, r)
	})
}

func (h Host) enable(a config.App, r ports.Registry) error {
	p, err := services.Render(a, r, h.TemplatesDir, h.Manager.StateDir)
	if err != nil {
		return err
	}
	if err := h.project(a); err != nil {
		return err
	}
	if err := h.ensureUser(a); err != nil {
		return err
	}
	if err := h.permissions(a); err != nil {
		return err
	}
	if !h.DryRun {
		if a.Web.Driver == "octane" {
			info, err := os.Stat(h.path("/usr/local/bin/rr"))
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
				return fmt.Errorf("shared RoadRunner is missing or not executable; run abr setup before enabling Octane")
			}
			local := h.path(filepath.Join(a.Directory, "rr"))
			if _, err := os.Lstat(local); err == nil {
				resolved, err := filepath.EvalSymlinks(local)
				shared, sharedErr := filepath.EvalSymlinks(h.path("/usr/local/bin/rr"))
				if err != nil || sharedErr != nil || resolved != shared {
					return fmt.Errorf("%s: app-local rr overrides shared RoadRunner; remove that copy before enabling", a.Name)
				}
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		required := []string{".output/server/index.mjs"}
		if a.Type == "laravel" {
			required = []string{"artisan", "vendor/autoload.php", "public/index.php", ".env"}
		}
		for _, path := range required {
			if _, err := os.Stat(h.path(filepath.Join(a.Directory, path))); err != nil {
				return fmt.Errorf("%s: missing %s; run abr deploy first: %w", a.Name, path, err)
			}
		}
	}
	if err := h.apply(a, r, p); err != nil {
		return err
	}
	if h.DryRun {
		h.say("Enable preview complete for %s", a.Name)
	} else {
		h.say("Enabled %s", a.Name)
	}
	return nil
}

func stopUnits(units []string) []string {
	all := append([]string{}, units...)
	for _, unit := range units {
		if strings.HasSuffix(unit, "-scheduler.timer") {
			all = append(all, strings.TrimSuffix(unit, ".timer")+".service")
		}
	}
	slices.SortFunc(all, func(a, b string) int {
		// Stop timers before their jobs, so no new job starts during shutdown.
		at, bt := strings.HasSuffix(a, ".timer"), strings.HasSuffix(b, ".timer")
		if at != bt {
			if at {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	return slices.Compact(all)
}

func (h Host) apply(a config.App, r ports.Registry, p services.Plan) (result error) {
	old, existed, err := h.loadManifest(a)
	if err != nil {
		return err
	}
	next := manifest{Version: 1, App: a.Name, User: a.User, Units: p.Units, FPM: p.FPM}
	for _, f := range p.Files {
		next.Files = append(next.Files, f.Path)
	}
	paths := append(append([]string{}, old.Files...), next.Files...)
	slices.Sort(paths)
	paths = slices.Compact(paths)
	// Back up every affected file before applying a reversible configuration change.
	backup := map[string]*services.File{}
	for _, path := range paths {
		if !h.fileAllowed(a, path) {
			return fmt.Errorf("invalid generated path")
		}
		if err := h.checkFile(path); err != nil {
			return err
		}
		data, err := h.read(path)
		if os.IsNotExist(err) {
			backup[path] = nil
			continue
		}
		if err != nil {
			return err
		}
		info, err := os.Stat(h.path(path))
		if err != nil {
			return err
		}
		backup[path] = &services.File{Path: path, Data: data, Mode: info.Mode().Perm()}
	}
	if !h.DryRun {
		pending := next
		pending.Files = paths
		pending.Units = stopUnits(append(append([]string{}, old.Units...), next.Units...))
		pending.FPM = old.FPM || next.FPM
		if err := h.saveManifest(pending); err != nil {
			return err
		}
		defer func() {
			if result == nil {
				return
			}
			h.say("Configuration failed; restoring previous managed files and services")
			var cleanup []error
			if len(next.Units) > 0 {
				cleanup = append(cleanup, h.stopAndDisable(next.Units))
			}
			for _, path := range paths {
				f := backup[path]
				if f == nil {
					cleanup = append(cleanup, h.removeFile(path))
				} else {
					cleanup = append(cleanup, h.write(path, f.Data, f.Mode))
				}
			}
			cleanup = append(cleanup, h.command("systemctl", "daemon-reload"))
			if p.FPM || old.FPM {
				cleanup = append(cleanup, h.command("systemctl", "reload", "php"+services.PHPVersion+"-fpm"))
			}
			if old.Enabled && len(old.Units) > 0 {
				cleanup = append(cleanup, h.command("systemctl", append([]string{"enable", "--now"}, old.Units...)...))
			}
			cleanup = append(cleanup, h.command("systemctl", "reload", "caddy"))
			cleanupError := errors.Join(cleanup...)
			if cleanupError == nil {
				if existed {
					cleanupError = h.saveManifest(old)
				} else {
					cleanupError = h.removeFile(h.manifestPath(a.Name))
				}
			}
			if cleanupError != nil {
				result = errors.Join(result, fmt.Errorf("rollback incomplete; reservations and recovery manifest retained: %w", cleanupError))
			}
		}()
	}
	if len(old.Units) > 0 {
		if err := h.stopAndDisable(old.Units); err != nil {
			return err
		}
	}
	for _, f := range p.Files {
		if err := h.write(f.Path, f.Data, f.Mode); err != nil {
			return err
		}
	}
	for _, path := range old.Files {
		if !slices.Contains(next.Files, path) {
			if err := h.removeFile(path); err != nil {
				return err
			}
		}
	}
	if err := h.command("systemctl", "daemon-reload"); err != nil {
		return err
	}
	var unitFiles []string
	for _, f := range p.Files {
		if strings.HasSuffix(f.Path, ".service") || strings.HasSuffix(f.Path, ".timer") {
			unitFiles = append(unitFiles, f.Path)
		}
	}
	if len(unitFiles) > 0 {
		if err := h.command("systemd-analyze", append([]string{"verify"}, unitFiles...)...); err != nil {
			return err
		}
	}
	if old.FPM || p.FPM {
		if err := h.command("/usr/sbin/php-fpm"+services.PHPVersion, "--test"); err != nil {
			return err
		}
	}
	if err := h.command("caddy", "validate", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"); err != nil {
		return err
	}
	allUnits := stopUnits(next.Units)
	// Stop managed processes before checking reservations, including after a redeploy.
	if len(allUnits) > 0 {
		for _, unit := range allUnits {
			if err := h.stopUnit(unit); err != nil {
				return err
			}
		}
	}
	if !h.DryRun {
		for _, purpose := range a.Endpoints() {
			port, _ := r.Lookup(a.Name, purpose)
			if err := ports.CheckAvailable(port); err != nil {
				return fmt.Errorf("%s/%s at %d: %w", a.Name, purpose, port, err)
			}
		}
	}
	if p.FPM || old.FPM {
		if err := h.command("systemctl", "reload", "php"+services.PHPVersion+"-fpm"); err != nil {
			return err
		}
	}
	if len(next.Units) > 0 {
		if err := h.command("systemctl", append([]string{"enable", "--now"}, next.Units...)...); err != nil {
			return err
		}
	}
	if err := h.ready(a, r, next); err != nil {
		return err
	}
	if err := h.command("systemctl", "reload", "caddy"); err != nil {
		return err
	}
	next.Enabled = true
	return h.saveManifest(next)
}

func (h Host) ready(a config.App, r ports.Registry, m manifest) error {
	if h.DryRun {
		h.say("Would verify active services and loopback listeners for %s", a.Name)
		return nil
	}
	if m.FPM {
		if err := h.command("systemctl", "is-active", "--quiet", "php"+services.PHPVersion+"-fpm"); err != nil {
			return err
		}
	}
	for _, unit := range m.Units {
		if err := h.command("systemctl", "is-active", "--quiet", unit); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(15 * time.Second)
	if m.FPM {
		for {
			connection, err := net.DialTimeout("unix", h.path("/run/php/abr-"+a.Name+".sock"), 250*time.Millisecond)
			if err == nil {
				connection.Close()
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("FPM socket did not become available for %s", a.Name)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	for _, purpose := range a.Endpoints() {
		port, _ := r.Lookup(a.Name, purpose)
		for {
			connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 250*time.Millisecond)
			if err == nil {
				connection.Close()
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s/%s did not listen at 127.0.0.1:%d; inspect abr logs", a.Name, purpose, port)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	// Recheck after waiting for sockets; an initial active state can precede
	// an immediate process failure during startup.
	for _, unit := range m.Units {
		if err := h.command("systemctl", "is-active", "--quiet", unit); err != nil {
			return err
		}
	}
	return nil
}

func (h Host) Disable(name string) error {
	return h.locked(func() error {
		a, _, err := h.application(name)
		if err != nil {
			return err
		}
		return h.disable(a)
	})
}

func (h Host) disable(a config.App) error {
	m, exists, err := h.loadManifest(a)
	if err != nil {
		return err
	}
	if !exists {
		h.say("%s has no installed managed services", a.Name)
		return nil
	}
	for _, path := range m.Files {
		if err := h.checkFile(path); err != nil {
			return err
		}
	}
	if len(m.Units) > 0 {
		if err := h.stopAndDisable(m.Units); err != nil {
			return err
		}
	}
	for _, path := range m.Files {
		if strings.HasPrefix(path, "/etc/caddy/") || strings.HasPrefix(path, "/etc/php/") {
			if err := h.removeFile(path); err != nil {
				return err
			}
		}
	}
	if m.FPM {
		if err := h.command("/usr/sbin/php-fpm"+services.PHPVersion, "--test"); err != nil {
			return err
		}
		if err := h.command("systemctl", "reload", "php"+services.PHPVersion+"-fpm"); err != nil {
			return err
		}
	}
	if err := h.command("caddy", "validate", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"); err != nil {
		return err
	}
	if err := h.command("systemctl", "reload", "caddy"); err != nil {
		return err
	}
	m.Enabled = false
	if err := h.saveManifest(m); err != nil {
		return err
	}
	if h.DryRun {
		h.say("Disable preview complete for %s", a.Name)
	} else {
		h.say("Disabled %s; reservations retained", a.Name)
	}
	return nil
}

func (h Host) stopUnit(unit string) error {
	if !h.DryRun {
		output, err := h.run("Check managed unit activity "+unit, Command{Name: "systemctl", Args: []string{"show", "--property=ActiveState", "--value", unit}, Private: true})
		if err != nil {
			return err
		}
		switch strings.TrimSpace(string(output)) {
		case "inactive", "failed":
			return nil // stop does not load a never-started unit, and can fail with exit 5.
		case "active", "activating", "deactivating", "reloading", "refreshing", "maintenance":
		default:
			return fmt.Errorf("unexpected activity state for managed unit %s", unit)
		}
	}
	return h.command("systemctl", "stop", unit)
}

func (h Host) stopAndDisable(units []string) error {
	for _, unit := range stopUnits(units) {
		if !h.DryRun {
			output, err := h.run("Check managed unit "+unit, Command{Name: "systemctl", Args: []string{"show", "--property=LoadState", "--value", unit}, Private: true})
			if err != nil {
				return err
			}
			if strings.TrimSpace(string(output)) == "not-found" {
				continue
			}
		}
		if err := h.stopUnit(unit); err != nil {
			return err
		}
		if slices.Contains(units, unit) {
			if err := h.command("systemctl", "disable", unit); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h Host) Remove(name string) error {
	return h.remove(name, false)
}

// Purge permanently deletes the registered app's data as well as its services.
func (h Host) Purge(name string, confirmed bool) error {
	if !confirmed && !h.DryRun {
		return fmt.Errorf("full removal deletes project files, uploads and database data; use --purge --yes to confirm")
	}
	return h.remove(name, true)
}

func (h Host) remove(name string, purge bool) error {
	return h.locked(func() error {
		a, r, err := h.application(name)
		if err != nil {
			return err
		}
		var cleanup purgePlan
		if purge {
			cleanup, err = h.planPurge(a)
		} else {
			err = h.project(a)
		}
		if err != nil {
			return err
		}
		m, _, err := h.loadManifest(a)
		if err != nil {
			return err
		}
		if err := h.disable(a); err != nil {
			return err
		}
		if purge {
			for _, unit := range stopUnits(m.Units) {
				if !h.DryRun {
					output, err := h.run("Check managed unit activity "+unit, Command{Name: "systemctl", Args: []string{"show", "--property=ActiveState", "--value", unit}, Private: true})
					if err != nil {
						return err
					}
					// Healthy inactive units can be unloaded between show and
					// reset-failed. Only failed units need their retained state cleared.
					if strings.TrimSpace(string(output)) != "failed" {
						continue
					}
				}
				if err := h.command("systemctl", "reset-failed", unit); err != nil {
					return err
				}
			}
		}
		if !h.DryRun {
			for _, assignment := range r.Assignments {
				if assignment.App == a.Name {
					if err := ports.CheckAvailable(assignment.Port); err != nil {
						return fmt.Errorf("refusing to release occupied reservation %d: %w", assignment.Port, err)
					}
				}
			}
		}
		for _, path := range m.Files {
			if err := h.checkFile(path); err != nil {
				return err
			}
			if err := h.removeFile(path); err != nil {
				return err
			}
		}
		if err := h.command("systemctl", "daemon-reload"); err != nil {
			return err
		}
		if err := h.removeUser(a, purge); err != nil {
			return err
		}
		if purge {
			if err := h.purgeData(a, cleanup); err != nil {
				return err
			}
		}
		if !h.DryRun {
			if err := h.Manager.Remove(a.Name); err != nil {
				return err
			}
		}
		if err := h.removeFile(h.manifestPath(a.Name)); err != nil {
			return err
		}
		if purge {
			for _, path := range []string{h.credentialsPath(a.Name), h.userPath(a)} {
				if err := h.removeFile(path); err != nil {
					return err
				}
			}
		}
		if h.DryRun {
			h.say("Would unregister %s and release its ports after verifying services stopped", a.Name)
		} else if purge {
			h.say("Fully removed %s: project, uploads, managed database/account, home, credentials, history, services and port reservations deleted", a.Name)
		} else {
			h.say("Removed %s; project files, home directory, database and credentials retained", a.Name)
		}
		return nil
	})
}

func (h Host) removeUser(a config.App, purge bool) error {
	if h.DryRun {
		h.say("Would remove the recorded managed Ubuntu account %s", a.User)
		return nil
	}
	data, err := h.read(h.userPath(a))
	if os.IsNotExist(err) {
		if purge {
			// A config-only registration has no account to remove. Never
			// treat an unrecorded existing account/home/group as managed.
			if _, exists, lookupErr := h.passwd(a.User); lookupErr != nil {
				return lookupErr
			} else if !exists {
				if _, homeErr := os.Lstat(h.path("/var/lib/abr-users/" + a.User)); os.IsNotExist(homeErr) {
					_, groupErr := h.run("Check unrecorded Ubuntu group "+a.User, Command{Name: "getent", Args: []string{"group", a.User}, Private: true})
					var exit interface{ ExitCode() int }
					if errors.As(groupErr, &exit) && exit.ExitCode() == 2 {
						return nil
					}
					if groupErr != nil {
						return groupErr
					}
				}
			}
		}
		return fmt.Errorf("no ownership record for user %s; account will not be removed", a.User)
	}
	if err != nil {
		return err
	}
	var record userRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	if record.App != a.Name || record.User != a.User || record.Home != "/var/lib/abr-users/"+a.User || record.UID == "" {
		return fmt.Errorf("invalid Ubuntu account ownership record")
	}
	entry, exists, err := h.passwd(a.User)
	if err != nil {
		return err
	}
	if exists {
		parts := strings.Split(entry, ":")
		if !validRuntimeAccount(parts, a.User) || parts[2] != record.UID || parts[4] != "abr-"+a.Name || parts[5] != record.Home {
			return fmt.Errorf("refusing to delete changed Ubuntu account %s", a.User)
		}
		output, err := h.run("Verify no remaining processes for "+a.User, Command{Name: "pgrep", Args: []string{"-u", record.UID}, Private: true})
		var exit interface{ ExitCode() int }
		if err == nil || len(output) > 0 {
			return fmt.Errorf("%s still has processes (including possibly draining FPM requests); retry removal after they exit", a.User)
		}
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return err
		}
		if purge {
			// Keep group identity and ownership records until cleanup completes,
			// including retries after userdel or filesystem failures.
			record.GID = parts[3]
			data, err := json.Marshal(record)
			if err != nil {
				return err
			}
			if err := h.write(h.userPath(a), data, 0600); err != nil {
				return err
			}
		}
		// Linux can reuse a deleted UID. Retained secrets must not become
		// readable by the next application assigned that UID.
		for _, path := range []string{a.Directory, record.Home} {
			if _, err := os.Lstat(h.path(path)); os.IsNotExist(err) && purge {
				continue
			}
			if err := h.command("chown", "-hR", "root:root", "--", path); err != nil {
				return err
			}
		}
		if err := h.gitAccess(a, true); err != nil {
			return err
		}
		if err := h.composerAccess(a, true); err != nil {
			return err
		}
		if err := h.command("userdel", a.User); err != nil {
			return err
		}
	}
	if purge {
		if err := h.removePrivateGroup(a, record); err != nil {
			return err
		}
		return h.forgetAppAccess(record)
	}
	return h.removeFile(h.userPath(a))
}

func (h Host) Status(name string) error {
	if err := h.guard(); err != nil {
		return err
	}
	a, _, err := h.application(name)
	if err != nil {
		return err
	}
	m, exists, err := h.loadManifest(a)
	if err != nil {
		return err
	}
	if !exists {
		h.say("%s: registered, no managed services installed", name)
		return nil
	}
	h.say("%s: enabled=%t, user=%s", name, m.Enabled, a.User)
	units := append([]string{}, m.Units...)
	if m.FPM {
		units = append(units, "php"+services.PHPVersion+"-fpm")
	}
	if len(units) == 0 {
		return nil
	}
	return h.command("systemctl", append([]string{"status", "--no-pager", "--full"}, units...)...)
}

func (h Host) Logs(name, service string, follow bool) error {
	if err := h.guard(); err != nil {
		return err
	}
	a, _, err := h.application(name)
	if err != nil {
		return err
	}
	m, exists, err := h.loadManifest(a)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%s has no installed managed services", name)
	}
	units, err := selectedUnits(a, m, service)
	if err != nil {
		return err
	}
	units = stopUnits(units) // Include scheduler job output, not just timer events.
	args := []string{"--no-pager", "-n", "100"}
	if follow {
		args = append(args, "--follow")
	}
	for _, unit := range units {
		args = append(args, "--unit", unit)
	}
	if len(units) == 0 {
		return fmt.Errorf("no journal units for %s", name)
	}
	return h.command("journalctl", args...)
}

func selectedUnits(a config.App, m manifest, service string) ([]string, error) {
	if service == "web" && m.FPM {
		return []string{"php" + services.PHPVersion + "-fpm"}, nil
	}
	var units []string
	for _, unit := range m.Units {
		short := strings.TrimPrefix(unit, "abr-"+a.Name+"-")
		short = strings.TrimSuffix(strings.TrimSuffix(short, ".service"), ".timer")
		if service == "" || service == short || (service == "queue" && strings.HasPrefix(short, "queue@")) || (service == "web" && (short == "nuxt" || short == "octane")) {
			units = append(units, unit)
		}
	}
	if service == "" && m.FPM {
		units = append(units, "php"+services.PHPVersion+"-fpm")
	}
	if len(units) == 0 {
		return nil, fmt.Errorf("unknown or disabled service %q for %s", service, a.Name)
	}
	return units, nil
}

func (h Host) Restart(name, service string) error {
	if service == "" {
		return h.Enable(name)
	}
	return h.locked(func() error {
		a, r, err := h.application(name)
		if err != nil {
			return err
		}
		m, exists, err := h.loadManifest(a)
		if err != nil {
			return err
		}
		if !exists || !m.Enabled {
			return fmt.Errorf("%s is not enabled", name)
		}
		units, err := selectedUnits(a, m, service)
		if err != nil {
			return err
		}
		verb := "restart"
		if service == "web" && m.FPM {
			verb = "reload"
		}
		if err := h.command("systemctl", append([]string{verb}, units...)...); err != nil {
			return err
		}
		return h.ready(a, r, m)
	})
}
