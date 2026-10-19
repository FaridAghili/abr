package host

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

const (
	swapFile  = "/swapfile"
	swapLabel = "abr-swap"
	swapUnit  = "/etc/systemd/system/swapfile.swap"
)

// Swap turns build and traffic spikes into slower paging instead of the OOM
// killer stopping MySQL or an app. Keep it small relative to a VPS disk.
func swapSize(memory uint64) uint64 {
	const gib = 1 << 30
	return min(max(memory/2, gib), 4*gib)
}

// configureSwap creates a labeled /swapfile only on a server without swap.
// Existing swap, including the provider's, is never changed or removed.
func (h Host) configureSwap() error {
	if err := h.setupConfig("swap-sysctl.conf.tmpl", "/etc/sysctl.d/99-abr-swap.conf", func() error {
		return h.command("sysctl", "-p", "/etc/sysctl.d/99-abr-swap.conf")
	}); err != nil {
		return err
	}
	if h.DryRun {
		h.say("Would create, label and enable %s when the server has no swap; existing swap is preserved", swapFile)
		return nil
	}
	if _, err := h.run("Check for a container without its own swap", Command{Name: "systemd-detect-virt", Args: []string{"--container", "--quiet"}, Private: true}); err == nil {
		h.say("Skipped swap: containers use the host's memory and swap limits")
		return nil
	} else if !exitStatus(err, 1) {
		return err
	}
	exists, managed, err := h.managedSwapFile()
	if err != nil {
		return err
	}
	active, err := h.activeSwap()
	if err != nil {
		return err
	}
	for _, name := range active {
		if name != swapFile || !managed {
			h.say("Existing swap %s preserved; no Abr swap file created", name)
			return nil
		}
	}
	if exists && !managed {
		return fmt.Errorf("%s exists without Abr's swap label; enable or remove it, then rerun setup", swapFile)
	}
	if !exists {
		created, err := h.createSwapFile()
		if err != nil || !created {
			return err
		}
	}
	if err := h.setupConfig("swapfile.swap.tmpl", swapUnit, func() error {
		if err := h.command("systemctl", "daemon-reload"); err != nil {
			return err
		}
		return h.command("systemctl", "enable", "--now", "swapfile.swap")
	}); err != nil {
		return err
	}
	active, err = h.activeSwap()
	if err != nil {
		return err
	}
	for _, name := range active {
		if name == swapFile {
			h.say("Swap enabled at %s", swapFile)
			return nil
		}
	}
	return fmt.Errorf("swap file %s did not become active", swapFile)
}

func (h Host) activeSwap() ([]string, error) {
	data, err := os.ReadFile(h.path("/proc/swaps"))
	if err != nil {
		return nil, err
	}
	var names []string
	for i, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); i > 0 && len(fields) > 0 {
			names = append(names, fields[0])
		}
	}
	return names, nil
}

func (h Host) managedSwapFile() (exists, managed bool, err error) {
	info, err := os.Lstat(h.path(swapFile))
	if os.IsNotExist(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || int(stat.Uid) != os.Geteuid() || info.Mode().Perm()&0077 != 0 {
		return true, false, fmt.Errorf("%s must be a private root-owned regular file", swapFile)
	}
	label, err := h.run("Read swap file label", Command{Name: "blkid", Args: []string{"--match-tag", "LABEL", "--output", "value", h.path(swapFile)}, Private: true})
	if exitStatus(err, 2) {
		return true, false, nil // No recognizable signature or label.
	}
	if err != nil {
		return true, false, err
	}
	return true, strings.TrimSpace(string(label)) == swapLabel, nil
}

// Build the file under a temporary name; only a complete, labeled swap area
// ever appears at /swapfile, so an interrupted setup can be retried.
func (h Host) createSwapFile() (created bool, result error) {
	meminfo, err := os.ReadFile(h.path("/proc/meminfo"))
	if err != nil {
		return false, err
	}
	match := regexp.MustCompile(`(?m)^MemTotal:\s+([0-9]+) kB$`).FindSubmatch(meminfo)
	if match == nil {
		return false, fmt.Errorf("cannot read total memory from /proc/meminfo")
	}
	kib, err := strconv.ParseUint(string(match[1]), 10, 64)
	if err != nil {
		return false, err
	}
	size := swapSize(kib << 10)
	var disk syscall.Statfs_t
	if err := syscall.Statfs(h.path("/"), &disk); err != nil {
		return false, err
	}
	// Leave room for deployments, logs and database growth.
	if free := disk.Bavail * uint64(disk.Bsize); free < size+4<<30 {
		h.say("Skipped swap: a %d MiB swap file needs %d MiB free disk space; %d MiB available", size>>20, (size+4<<30)>>20, free>>20)
		return false, nil
	}
	partial := h.path(swapFile + ".abr-partial")
	if err := os.Remove(partial); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	f, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	defer func() {
		if result != nil {
			_ = os.Remove(partial)
		}
	}()
	if err := h.command("fallocate", "--length", strconv.FormatUint(size, 10), partial); err != nil {
		return false, err
	}
	if err := h.command("mkswap", "--label", swapLabel, partial); err != nil {
		return false, err
	}
	if err := os.Rename(partial, h.path(swapFile)); err != nil {
		return false, err
	}
	h.say("Created %d MiB swap file %s", size>>20, swapFile)
	return true, nil
}

var baseSnap = regexp.MustCompile(`^(snapd|bare|core[0-9]*)$`)

// Snaps other than snapd itself and its base runtimes.
func applicationSnaps(list string) []string {
	var names []string
	lines := strings.Split(list, "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "Name ") {
		return nil
	}
	for _, line := range lines[1:] {
		if fields := strings.Fields(line); len(fields) > 0 && !baseSnap.MatchString(fields[0]) {
			names = append(names, fields[0])
		}
	}
	return names
}

// Packages apt would remove besides snapd, from an `apt-get -s purge` run.
func otherRemovals(simulation string) []string {
	var names []string
	for _, line := range strings.Split(simulation, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && (fields[0] == "Purg" || fields[0] == "Remv") && fields[1] != "snapd" {
			names = append(names, fields[1])
		}
	}
	return names
}

// removeSnapd purges an unused snapd and pins it so upgrades do not return it.
// Installed application snaps or dependent packages keep snapd in place.
func (h Host) removeSnapd() error {
	const pin = "/etc/apt/preferences.d/abr-no-snapd"
	pinData := []byte("Explanation: Managed by abr; snapd is not used on this server.\nPackage: snapd\nPin: release a=*\nPin-Priority: -10\n")
	if h.DryRun {
		h.say("Would remove snapd when no application snaps or dependent packages need it, then keep it from being reinstalled")
		return nil
	}
	status, err := h.run("Check installed snapd package", Command{Name: "dpkg-query", Args: []string{"-W", "-f=${Status}", "snapd"}, Private: true})
	if err != nil && !exitStatus(err, 1) {
		return err
	}
	if err != nil || strings.TrimSpace(string(status)) != "install ok installed" {
		return h.write(pin, pinData, 0644)
	}
	list, err := h.run("List installed snaps", Command{Name: "snap", Args: []string{"list"}, Private: true})
	if err != nil {
		return err
	}
	if snaps := applicationSnaps(string(list)); len(snaps) > 0 {
		h.say("Kept snapd: installed snaps %s still use it", strings.Join(snaps, ", "))
		return nil
	}
	simulation, err := h.run("Check packages that depend on snapd", Command{Name: "apt-get", Args: []string{"-s", "purge", "snapd"}, Env: []string{"DEBIAN_FRONTEND=noninteractive"}, Private: true})
	if err != nil {
		return err
	}
	if others := otherRemovals(string(simulation)); len(others) > 0 {
		h.say("Kept snapd: removing it would also remove %s", strings.Join(others, ", "))
		return nil
	}
	if _, err := h.run("Remove unused snapd", Command{Name: "apt-get", Args: []string{"-o", "DPkg::Lock::Timeout=120", "purge", "-y", "snapd"}, Env: []string{"DEBIAN_FRONTEND=noninteractive"}, Stream: true}); err != nil {
		return err
	}
	if err := h.write(pin, pinData, 0644); err != nil {
		return err
	}
	h.say("Removed unused snapd; an APT pin keeps it from being reinstalled")
	return nil
}
