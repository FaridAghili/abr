package host

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var caddyFixturePackage = []byte("disposable Debian package fixture")

func caddyFixtureMetadata() []byte {
	sum := sha256.Sum256(caddyFixturePackage)
	return []byte(fmt.Sprintf(`{"tag_name":"v2.11.7","assets":[{"name":"caddy_2.11.7_linux_amd64.deb","browser_download_url":"https://github.com/caddyserver/caddy/releases/download/v2.11.7/caddy_2.11.7_linux_amd64.deb","digest":"sha256:%s"}]}`, hex.EncodeToString(sum[:])))
}

func caddyFixtureDownload(url string, limit int64) ([]byte, error) {
	if url == caddyLatestURL {
		return caddyFixtureMetadata(), nil
	}
	if url == "https://github.com/caddyserver/caddy/releases/download/v2.11.7/caddy_2.11.7_linux_amd64.deb" {
		return caddyFixturePackage, nil
	}
	return nil, fmt.Errorf("unexpected fixture URL %s", url)
}

type caddyRunner struct {
	*fakeRunner
	installed string
	failure   string
}

func (r *caddyRunner) Run(c Command) ([]byte, error) {
	out, err := r.fakeRunner.Run(c)
	if err != nil {
		return out, err
	}
	if r.failure != "" && slices.Contains(c.Args, r.failure) {
		return nil, testExit(2)
	}
	switch {
	case c.Name == "dpkg-query":
		if r.installed == "" {
			return nil, testExit(1)
		}
		return []byte("install ok installed\n" + r.installed), nil
	case c.Name == "dpkg":
		if r.installed == "2.6.2" {
			return nil, testExit(1)
		}
	case filepath.Base(c.Name) == "caddy" && c.Args[0] == "version":
		return []byte("v2.11.7 h1:fixture\n"), nil
	}
	return out, nil
}

func TestCaddyReleaseRejectsUnstableOrUnverifiedAssets(t *testing.T) {
	for _, change := range []string{"", "draft", "prerelease", "tag", "asset", "url", "digest"} {
		t.Run(change, func(t *testing.T) {
			var metadata map[string]any
			if err := json.Unmarshal(caddyFixtureMetadata(), &metadata); err != nil {
				t.Fatal(err)
			}
			asset := metadata["assets"].([]any)[0].(map[string]any)
			switch change {
			case "draft", "prerelease":
				metadata[change] = true
			case "tag":
				metadata["tag_name"] = "v2.11.8-rc.1"
			case "asset":
				asset["name"] = "caddy_2.11.7_linux_arm64.deb"
			case "url":
				asset["browser_download_url"] = "https://example.invalid/caddy.deb"
			case "digest":
				asset["digest"] = ""
			}
			data, _ := json.Marshal(metadata)
			version, _, _, err := caddyRelease(data)
			if (err != nil) != (change != "") {
				t.Fatalf("release accepted=%v: %v", change, err)
			}
			if change == "" && version != "2.11.7" {
				t.Fatal(version)
			}
		})
	}
}

func TestCaddyInstallAndUpgrade(t *testing.T) {
	for _, installed := range []string{"", "2.6.2", "2.11.7", "2.12.0"} {
		t.Run(installed, func(t *testing.T) {
			h, fake, _, _ := fixture(t)
			if err := os.MkdirAll(h.Manager.StateDir, 0700); err != nil {
				t.Fatal(err)
			}
			r := &caddyRunner{fakeRunner: fake, installed: installed}
			h.Runner = r
			downloads := 0
			h.caddyFetch = func(url string, limit int64) ([]byte, error) { downloads++; return caddyFixtureDownload(url, limit) }
			if installed != "" {
				if err := h.write("/etc/caddy/Caddyfile", []byte(":80 { respond 404 }"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if err := h.installCaddy(); err != nil {
				t.Fatal(err)
			}
			var installedPackage, validated, restarted bool
			for _, c := range r.calls {
				if slices.Contains(c.Args, "validate") {
					validated = true
				}
				if c.Name == "apt-get" {
					installedPackage = true
					if installed != "" && !validated {
						t.Fatal("installed before validating existing config")
					}
					if !slices.Contains(c.Args, "Dpkg::Options::=--force-confold") {
						t.Fatal("configuration is not preserved")
					}
				}
				if c.Name == "systemctl" && slices.Contains(c.Args, "restart") {
					restarted = true
				}
			}
			upgrade := installed == "" || installed == "2.6.2"
			if installedPackage != upgrade || restarted != (installed == "2.6.2") {
				t.Fatalf("install=%v restart=%v", installedPackage, restarted)
			}
			wantDownloads := 1
			if upgrade {
				wantDownloads = 2
			}
			if downloads != wantDownloads {
				t.Fatalf("downloads=%d", downloads)
			}
			entries, err := os.ReadDir(h.Manager.StateDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".caddy-") {
					t.Fatal("staging files remain")
				}
			}
		})
	}
}

func TestCaddyUpgradeStopsOnFailure(t *testing.T) {
	for _, failure := range []string{"download", "checksum", "--extract", "version", "validate", "install", "restart", "is-active"} {
		t.Run(failure, func(t *testing.T) {
			h, fake, _, _ := fixture(t)
			if err := os.MkdirAll(h.Manager.StateDir, 0700); err != nil {
				t.Fatal(err)
			}
			r := &caddyRunner{fakeRunner: fake, installed: "2.6.2", failure: failure}
			h.Runner = r
			h.caddyFetch = func(url string, limit int64) ([]byte, error) {
				if failure == "download" {
					return nil, fmt.Errorf("fixture download failed")
				}
				if failure == "checksum" && url != caddyLatestURL {
					return []byte("corrupt"), nil
				}
				return caddyFixtureDownload(url, limit)
			}
			if err := h.write("/etc/caddy/Caddyfile", []byte("fixture configuration"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := h.installCaddy(); err == nil {
				t.Fatal("failure reported success")
			}
			beforeInstall := slices.Contains([]string{"download", "checksum", "--extract", "version", "validate"}, failure)
			for _, c := range r.calls {
				if beforeInstall && c.Name == "apt-get" {
					t.Fatal("installed after failed preflight")
				}
			}
			data, err := h.read("/etc/caddy/Caddyfile")
			if err != nil || string(data) != "fixture configuration" {
				t.Fatal("changed configuration")
			}
		})
	}
}

func TestCaddyDryRunDoesNotDownload(t *testing.T) {
	h, r, _, _ := fixture(t)
	h.DryRun = true
	h.caddyFetch = func(string, int64) ([]byte, error) { t.Fatal("dry run downloaded"); return nil, nil }
	if err := h.installCaddy(); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 0 {
		t.Fatal("dry run ran commands")
	}
}
