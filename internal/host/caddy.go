package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const caddyLatestURL = "https://api.github.com/repos/caddyserver/caddy/releases/latest"

func exitStatus(err error, code int) bool {
	var exit interface{ ExitCode() int }
	return errors.As(err, &exit) && exit.ExitCode() == code
}

// Use the upstream native package so dpkg owns the binary, user and service.
func (h Host) installCaddy() error {
	h.say("Check latest stable Caddy on GitHub; install newer native package with SHA256 verification")
	if h.DryRun {
		return nil
	}
	download := h.caddyFetch
	if download == nil {
		download = fetch
	}
	metadata, err := download(caddyLatestURL, 2<<20)
	if err != nil {
		return err
	}
	version, url, digest, err := caddyRelease(metadata)
	if err != nil {
		return err
	}
	installed, err := h.run("Check installed Caddy package", Command{Name: "dpkg-query", Args: []string{"-W", "-f=${Status}\n${Version}", "caddy"}, Private: true})
	if err != nil && !exitStatus(err, 1) {
		return err
	}
	if err == nil && strings.HasPrefix(string(installed), "install ok installed\n") {
		current := strings.TrimSpace(strings.TrimPrefix(string(installed), "install ok installed\n"))
		_, err := h.run("Compare installed Caddy with GitHub stable "+version, Command{Name: "dpkg", Args: []string{"--compare-versions", current, "ge", version}, Private: true})
		if err == nil {
			h.say("Caddy package %s is already at or above stable %s", current, version)
			return nil
		}
		if !exitStatus(err, 1) {
			return err
		}
	}
	deb, err := download(url, 100<<20)
	if err != nil {
		return err
	}
	if err := verifySHA256(deb, strings.TrimPrefix(digest, "sha256:")); err != nil {
		return fmt.Errorf("Caddy: %w", err)
	}
	if err := h.trustedDirectory(h.path(h.Manager.StateDir)); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(h.path(h.Manager.StateDir), ".caddy-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	packagePath := filepath.Join(dir, "caddy.deb")
	if err := h.write(packagePath, deb, 0644); err != nil {
		return err
	}
	stage := filepath.Join(dir, "stage")
	if err := h.command("dpkg-deb", "--extract", packagePath, stage); err != nil {
		return err
	}
	binary := filepath.Join(stage, "usr/bin/caddy")
	if err := h.verifyCaddyVersion(binary, version); err != nil {
		return err
	}
	_, err = os.Stat(h.path("/etc/caddy/Caddyfile"))
	existing := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if existing {
		// Reject incompatible custom configurations before replacing the service.
		if err := h.command(binary, "validate", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"); err != nil {
			return err
		}
	}
	if _, err := h.run("Install Caddy stable "+version, Command{Name: "apt-get", Args: []string{"-o", "DPkg::Lock::Timeout=120", "-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold", "install", "-y", packagePath}, Env: []string{"DEBIAN_FRONTEND=noninteractive"}, Stream: true}); err != nil {
		return err
	}
	if err := h.verifyCaddyVersion("/usr/bin/caddy", version); err != nil {
		return err
	}
	if existing {
		if err := h.command("systemctl", "daemon-reload"); err != nil {
			return err
		}
		if err := h.command("systemctl", "restart", "caddy"); err != nil {
			return err
		}
		return h.command("systemctl", "is-active", "--quiet", "caddy")
	}
	return nil
}

func (h Host) verifyCaddyVersion(binary, version string) error {
	out, err := h.run("Verify Caddy "+version, Command{Name: binary, Args: []string{"version"}, Private: true})
	if err != nil {
		return err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 || fields[0] != "v"+version {
		return fmt.Errorf("Caddy executable does not match release %s", version)
	}
	return nil
}

func caddyRelease(metadata []byte) (version, url, digest string, err error) {
	var release struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err = json.Unmarshal(metadata, &release); err != nil {
		return
	}
	if release.Draft || release.Prerelease || !regexp.MustCompile(`^v2\.[0-9]+\.[0-9]+$`).MatchString(release.Tag) {
		err = fmt.Errorf("unexpected or unstable Caddy release")
		return
	}
	version = strings.TrimPrefix(release.Tag, "v")
	name := "caddy_" + version + "_linux_amd64.deb"
	for _, asset := range release.Assets {
		if asset.Name != name {
			continue
		}
		url = "https://github.com/caddyserver/caddy/releases/download/" + release.Tag + "/" + name
		if asset.URL != url || !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(asset.Digest) {
			err = fmt.Errorf("unexpected Caddy asset URL or missing SHA256")
			return
		}
		return version, url, asset.Digest, nil
	}
	err = fmt.Errorf("Caddy release has no %s asset", name)
	return
}
