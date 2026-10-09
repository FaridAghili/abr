package host

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"abr/internal/config"
	"abr/internal/manager"
	"abr/internal/ports"
)

type doctorEndpoint struct {
	app, purpose, unit string
	port               int
	enabled, installed bool
}

type doctorUnit struct{ load, active, group string }
type unverifiedPort struct{ detail string }

func (e *unverifiedPort) Error() string { return e.detail }

type doctorListener struct {
	address string
	pids    []string
}

// Doctor is read-only. A listener is expected only when every socket owner
// belongs to the endpoint's recorded, active systemd service (or its children).
func (h Host) Doctor() error {
	c, r, err := h.Manager.Snapshot()
	if err == nil {
		err = manager.CheckReservations(c, r)
	}
	if err != nil {
		h.say("ERROR: configuration and reservations: %v", err)
		return err
	}
	h.say("OK: configuration and port reservations")
	count := 0
	for _, app := range c.Apps {
		count += len(app.Endpoints())
	}
	if count == 0 {
		h.say("OK: no configured TCP app endpoints to check")
		return nil
	}
	if h.DryRun {
		h.say("Would check %d app endpoints against live listeners and their systemd services; runtime health was not checked", count)
		return nil
	}
	if runtime.GOOS != "linux" && h.check == nil {
		return h.portableDoctor(c, r)
	}
	if err := h.guard(); err != nil {
		return fmt.Errorf("cannot verify live app ports: %w", err)
	}

	var endpoints []doctorEndpoint
	var units []string
	var problems []error
	unverifiedCount := 0
	for _, app := range c.Apps {
		m, exists, err := h.loadManifest(app)
		if err != nil {
			h.say("ERROR: %s: %v", app.Name, err)
			problems = append(problems, err)
			continue
		}
		for _, purpose := range app.Endpoints() {
			unit := endpointUnit(app.Name, purpose)
			port, _ := r.Lookup(app.Name, purpose)
			installed := exists && slices.Contains(m.Units, unit)
			endpoints = append(endpoints, doctorEndpoint{app.Name, purpose, unit, port, m.Enabled, installed})
			if installed && !slices.Contains(units, unit) {
				units = append(units, unit)
			}
		}
	}
	states := map[string]doctorUnit{}
	if len(units) > 0 {
		data, err := h.run("Check app endpoint services", Command{Name: "systemctl", Args: append([]string{"show", "--no-pager", "--property=Id,LoadState,ActiveState,ControlGroup"}, units...), Private: true})
		if err != nil {
			return fmt.Errorf("cannot verify systemd service state: %w", err)
		}
		states, err = parseDoctorUnits(string(data))
		if err != nil {
			return err
		}
	}
	data, err := h.run("Inspect TCP listener ownership", Command{Name: "ss", Args: []string{"-H", "-ltnp"}, Private: true})
	if err != nil {
		return fmt.Errorf("cannot verify TCP listener ownership: %w", err)
	}
	listeners, err := parseDoctorListeners(string(data))
	if err != nil {
		return err
	}
	for _, ep := range endpoints {
		detail, err := h.checkDoctorEndpoint(ep, states[ep.unit], listeners[ep.port])
		level := "OK"
		if err != nil {
			level = "ERROR"
			var unverified *unverifiedPort
			if errors.As(err, &unverified) {
				level = "UNVERIFIED"
				unverifiedCount++
			}
			problems = append(problems, err)
		}
		h.say("%s: %s/%s at %d: %s", level, ep.app, ep.purpose, ep.port, detail)
	}
	if len(problems) > 0 {
		return fmt.Errorf("checks need attention: %d error(s), %d unverified check(s); see details above", len(problems)-unverifiedCount, unverifiedCount)
	}
	h.say("OK: all %d app endpoint checks passed (point-in-time; HTTPS, databases and other services are not checked)", len(endpoints))
	return nil
}

func endpointUnit(app, purpose string) string {
	suffix := ""
	switch purpose {
	case "octane-http", "roadrunner-rpc":
		suffix = "octane"
	case "nuxt-http":
		suffix = "nuxt"
	case "inertia-ssr":
		suffix = "inertia-ssr"
	case "nightwatch-ingest":
		suffix = "nightwatch"
	}
	return "abr-" + app + "-" + suffix + ".service"
}

func (h Host) checkDoctorEndpoint(ep doctorEndpoint, state doctorUnit, listeners []doctorListener) (string, error) {
	bad := func(detail string) (string, error) { return detail, errors.New(detail) }
	unknown := func(detail string) (string, error) { return detail, &unverifiedPort{detail} }
	if ep.installed && (state.load == "" || state.active == "") {
		return unknown("cannot verify services: no systemd state returned for " + ep.unit)
	}
	if len(listeners) > 0 {
		if ep.installed && state.group == "" && (state.load != "loaded" || state.active != "active") {
			return bad("port is occupied while expected service " + ep.unit + " is " + state.load + "/" + state.active + "; inspect app service status")
		}
		if ep.installed && (state.group == "" || state.group == "/" || !strings.HasPrefix(state.group, "/") || filepath.Clean(state.group) != state.group) {
			return unknown("cannot verify listener ownership: expected service " + ep.unit + " has no usable control group (" + state.load + "/" + state.active + ")")
		}
		for _, listener := range listeners {
			if len(listener.pids) == 0 {
				return unknown("cannot verify listener owner at " + listener.address + "; retry with sudo")
			}
			for _, pid := range listener.pids {
				data, err := os.ReadFile(h.path(filepath.Join("/proc", pid, "cgroup")))
				if err != nil {
					return unknown("cannot verify listener PID " + pid + "; process may have exited, retry the check")
				}
				group := ""
				for _, line := range strings.Split(string(data), "\n") {
					if strings.HasPrefix(line, "0::") {
						group = strings.TrimPrefix(line, "0::")
					}
				}
				if group == "" {
					return unknown("cannot verify listener PID " + pid + ": unified cgroup information is unavailable")
				}
				if !ep.installed || state.group == "" || (group != state.group && !strings.HasPrefix(group, state.group+"/")) {
					return bad(fmt.Sprintf("port conflict: listener PID %s at %s is outside expected service %s", pid, listener.address, ep.unit))
				}
			}
		}
		if !ep.enabled {
			return bad("app is recorded as disabled, but its service is still listening; use abr disable " + ep.app + " or abr enable " + ep.app)
		}
		if state.load != "loaded" || state.active != "active" {
			return bad("expected service " + ep.unit + " is " + state.load + "/" + state.active + "; retry or inspect service status")
		}
		return "listening, owned by active " + ep.unit, nil
	}
	probe := h.Manager.Probe
	if probe == nil {
		probe = ports.CheckAvailable
	}
	if err := probe(ep.port); err != nil {
		return unknown("cannot confirm port availability: " + err.Error() + "; retry the check")
	}
	if ep.enabled {
		if !ep.installed {
			return bad("enabled app is missing its recorded endpoint service; deploy " + ep.app + " to apply configuration")
		}
		return bad("expected listener is missing (" + ep.unit + " is " + state.load + "/" + state.active + "); inspect app service status")
	}
	reason := "app disabled"
	if !ep.installed {
		reason = "service not deployed"
	}
	return "available (" + reason + ")", nil
}

func parseDoctorUnits(data string) (map[string]doctorUnit, error) {
	result := map[string]doctorUnit{}
	for _, block := range strings.Split(strings.TrimSpace(data), "\n\n") {
		values := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				return nil, fmt.Errorf("cannot verify services: malformed systemctl output")
			}
			values[key] = value
		}
		if values["Id"] == "" || values["LoadState"] == "" || values["ActiveState"] == "" {
			return nil, fmt.Errorf("cannot verify services: incomplete systemctl output")
		}
		result[values["Id"]] = doctorUnit{values["LoadState"], values["ActiveState"], values["ControlGroup"]}
	}
	return result, nil
}

var listenerPID = regexp.MustCompile(`\bpid=([1-9][0-9]*)(?:,|\))`)

func parseDoctorListeners(data string) (map[int][]doctorListener, error) {
	result := map[int][]doctorListener{}
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "LISTEN" {
			return nil, fmt.Errorf("cannot verify listeners: malformed ss output")
		}
		address := fields[3]
		i := strings.LastIndexByte(address, ':')
		if i < 0 {
			return nil, fmt.Errorf("cannot verify listener address")
		}
		port, err := strconv.Atoi(address[i+1:])
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("cannot verify listener port")
		}
		listener := doctorListener{address: address}
		for _, match := range listenerPID.FindAllStringSubmatch(strings.Join(fields[5:], " "), -1) {
			listener.pids = append(listener.pids, match[1])
		}
		result[port] = append(result[port], listener)
	}
	return result, nil
}

func (h Host) portableDoctor(c config.Config, r ports.Registry) error {
	h.say("Portable checks only: live systemd ownership and app health require sudo abr doctor on the Ubuntu host")
	var problems []error
	probe := h.Manager.Probe
	if probe == nil {
		probe = ports.CheckAvailable
	}
	for _, app := range c.Apps {
		for _, purpose := range app.Endpoints() {
			port, _ := r.Lookup(app.Name, purpose)
			if err := probe(port); err != nil {
				h.say("UNVERIFIED: %s/%s at %d: %v; listener ownership was not checked", app.Name, purpose, port, err)
				problems = append(problems, err)
			} else {
				h.say("OK: %s/%s at %d: available on this machine", app.Name, purpose, port)
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("portable port checks incomplete; verify listener ownership on the Ubuntu host")
	}
	return nil
}
