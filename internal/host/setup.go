package host

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"abr"
	"abr/internal/config"
	"abr/internal/services"
)

const DefaultRoadRunnerVersion = "latest"

type SetupOptions struct {
	RoadRunnerVersion             string
	SSHPort                       int // Zero discovers ports from the effective sshd configuration.
	AdminUser                     string
	NoFirewall, NoRedis, NoImages bool
}

func (h Host) Setup(o SetupOptions) error {
	if o.RoadRunnerVersion != "latest" && !regexp.MustCompile(`^\d{4}\.\d+\.\d+$`).MatchString(o.RoadRunnerVersion) {
		return fmt.Errorf("invalid RoadRunner version")
	}
	if o.SSHPort < 0 || o.SSHPort > 65535 {
		return fmt.Errorf("invalid SSH port")
	}
	return h.locked(func() error {
		templates, err := abr.TemplateFS()
		if err != nil {
			return err
		}
		if !h.DryRun {
			// Check bundled defaults before making package changes.
			if err := requireSetupTemplates(templates); err != nil {
				return err
			}
		}
		admin, err := h.sshPreflight(o.AdminUser)
		if err != nil {
			return err
		}
		if err := h.command("apt-get", "-o", "APT::Update::Error-Mode=any", "update"); err != nil {
			return err
		}
		packages := []string{"install", "-y", "ca-certificates", "curl", "gnupg", "git", "unzip", "xz-utils", "brotli", "acl", "build-essential", "openssh-server", "ufw", "fail2ban", "unattended-upgrades", "mysql-server", "ncdu", "libmagickcore-7.q16-10-extra", "librsvg2-bin"}
		for _, extension := range []string{"cli", "fpm", "bcmath", "curl", "gd", "imagick", "intl", "mbstring", "mysql", "redis", "xml", "zip", "excimer"} {
			packages = append(packages, "php"+services.PHPVersion+"-"+extension)
		}
		if !o.NoRedis {
			packages = append(packages, "redis-server")
		}
		if !o.NoImages {
			packages = append(packages, "imagemagick-7.q16", "gifsicle", "jpegoptim", "libavif-bin", "optipng", "pngquant", "webp")
		}
		if _, err := h.run("Install shared Ubuntu packages: "+strings.Join(packages[2:], ", "), Command{Name: "apt-get", Args: append([]string{"-o", "DPkg::Lock::Timeout=120"}, packages...), Env: []string{"DEBIAN_FRONTEND=noninteractive"}}); err != nil {
			return err
		}
		if err := h.command("update-alternatives", "--set", "php", "/usr/bin/php"+services.PHPVersion); err != nil {
			return err
		}
		for _, repo := range []struct{ name, keyURL, source string }{
			{"caddy", "https://dl.cloudsmith.io/public/caddy/stable/gpg.key", "Types: deb\nURIs: https://dl.cloudsmith.io/public/caddy/stable/deb/debian\nSuites: any-version\nComponents: main\nArchitectures: amd64\n"},
			{"node", "https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key", "Types: deb\nURIs: https://deb.nodesource.com/node_24.x\nSuites: nodistro\nComponents: main\nArchitectures: amd64\n"},
		} {
			var key []byte
			if !h.DryRun {
				armored, err := fetch(repo.keyURL, 1<<20)
				if err != nil {
					return err
				}
				key, err = h.run("Import "+repo.name+" repository signing key", Command{Name: "gpg", Args: []string{"--batch", "--dearmor"}, Input: armored, Private: true})
				if err != nil {
					return err
				}
			}
			if err := h.writeAPTRepository(repo.name, key, repo.source); err != nil {
				return err
			}
		}
		if err := h.write("/etc/apt/preferences.d/abr-node", []byte("Package: nodejs\nPin: origin deb.nodesource.com\nPin-Priority: 600\n"), 0644); err != nil {
			return err
		}
		if err := h.command("apt-get", "-o", "APT::Update::Error-Mode=any", "update"); err != nil {
			return err
		}
		if _, err := h.run("Install Caddy and shared Node 24/npm", Command{Name: "apt-get", Args: []string{"-o", "DPkg::Lock::Timeout=120", "install", "-y", "caddy", "nodejs"}, Env: []string{"DEBIAN_FRONTEND=noninteractive"}}); err != nil {
			return err
		}
		if err := h.installNPM(!o.NoImages); err != nil {
			return err
		}
		if err := h.installComposer(); err != nil {
			return err
		}
		if err := h.installRoadRunner(o.RoadRunnerVersion); err != nil {
			return err
		}
		if !h.DryRun {
			for _, dir := range []string{h.AppsDir, "/etc/caddy/abr.d", h.TemplatesDir} {
				if err := os.MkdirAll(h.path(dir), 0755); err != nil {
					return err
				}
			}
		}
		if err := h.installTemplates(templates); err != nil {
			return err
		}
		if _, err := h.read(h.Manager.ConfigPath); os.IsNotExist(err) || h.DryRun {
			data, err := config.Encode(config.Default())
			if err != nil {
				return err
			}
			if err := h.write(h.Manager.ConfigPath, data, 0600); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := h.configureCaddyImport(); err != nil {
			return err
		}
		if err := h.command("systemctl", "enable", "--now", "mysql", "php"+services.PHPVersion+"-fpm", "caddy"); err != nil {
			return err
		}
		if err := h.configureCaddyAdmin(); err != nil {
			return err
		}
		if err := h.configurePHP(); err != nil {
			return err
		}
		if err := h.hardenMySQL(); err != nil {
			return err
		}
		if !o.NoRedis {
			if err := h.configureRedis(); err != nil {
				return err
			}
		}
		if err := h.hardenSSH(admin); err != nil {
			return err
		}
		sshPorts, err := h.sshPorts(o.SSHPort)
		if err != nil {
			return err
		}
		if !o.NoFirewall {
			for _, port := range sshPorts {
				if err := h.command("ufw", "allow", port+"/tcp"); err != nil {
					return err
				}
			}
			for _, rule := range []string{"80/tcp", "443/tcp", "443/udp"} {
				if err := h.command("ufw", "allow", rule); err != nil {
					return err
				}
			}
			if err := h.command("ufw", "default", "deny", "incoming"); err != nil {
				return err
			}
			if err := h.command("ufw", "default", "allow", "outgoing"); err != nil {
				return err
			}
			if err := h.command("ufw", "--force", "enable"); err != nil {
				return err
			}
		}
		jail := "[sshd]\nenabled = true\nbackend = systemd\nport = " + strings.Join(sshPorts, ",") + "\n"
		if err := h.write("/etc/fail2ban/jail.d/abr-sshd.local", []byte(jail), 0644); err != nil {
			return err
		}
		if err := h.command("systemctl", "enable", "--now", "fail2ban"); err != nil {
			return err
		}
		if err := h.command("systemctl", "restart", "fail2ban"); err != nil {
			return err
		}
		if !h.DryRun {
			// systemctl returns before Fail2ban's control socket is ready.
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(h.path("/run/fail2ban/fail2ban.sock")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("Fail2ban control socket did not become ready")
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
		if err := h.command("fail2ban-client", "status", "sshd"); err != nil {
			return err
		}
		if err := h.setupConfig("automatic-updates.conf.tmpl", "/etc/apt/apt.conf.d/99-abr-updates", func() error {
			return h.command("systemctl", "enable", "--now", "apt-daily.timer", "apt-daily-upgrade.timer")
		}); err != nil {
			return err
		}
		if err := h.command("systemctl", "reload", "caddy"); err != nil {
			return err
		}
		if h.DryRun {
			h.say("Setup preview complete; no commands executed or files changed")
		} else {
			h.say("VPS setup complete; shared packages and RoadRunner installed")
		}
		return nil
	})
}

func (h Host) writeAPTRepository(name string, key []byte, source string) error {
	// Administrator-managed keys belong under /etc, separate from package keys
	// in /usr/share. Keep our directory root-owned and readable by APT's _apt user.
	keyPath := "/etc/apt/keyrings/abr/" + name + ".gpg"
	if err := h.write(keyPath, key, 0644); err != nil {
		return err
	}
	return h.write("/etc/apt/sources.list.d/abr-"+name+".sources", []byte(source+"Signed-By: "+keyPath+"\n"), 0644)
}

func fetch(url string, limit int64) ([]byte, error) {
	if !strings.HasPrefix(url, "https://") {
		return nil, fmt.Errorf("downloads require HTTPS")
	}
	client := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if request.URL.Scheme != "https" || len(via) >= 10 {
			return fmt.Errorf("unsafe or excessive download redirects")
		}
		return nil
	}}
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "abr/"+abr.Version)
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", url, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("download exceeds size limit: %s", url)
	}
	return data, nil
}

func (h Host) installRoadRunner(version string) error {
	h.say("Install shared RoadRunner %s with GitHub asset SHA256 verification", version)
	if h.DryRun {
		h.say("Would install /opt/roadrunner/%s/rr and link /usr/local/bin/rr", version)
		return nil
	}
	url := "https://api.github.com/repos/roadrunner-server/roadrunner/releases/tags/v" + version
	if version == "latest" {
		url = "https://api.github.com/repos/roadrunner-server/roadrunner/releases/latest"
	}
	data, err := fetch(url, 2<<20)
	if err != nil {
		return err
	}
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
	if err := json.Unmarshal(data, &release); err != nil {
		return err
	}
	resolved := strings.TrimPrefix(release.Tag, "v")
	if release.Draft || release.Prerelease || !regexp.MustCompile(`^\d{4}\.\d+\.\d+$`).MatchString(resolved) || (version != "latest" && resolved != version) {
		return fmt.Errorf("unexpected or unstable RoadRunner release")
	}
	version = resolved
	name := "roadrunner-" + version + "-linux-amd64.tar.gz"
	for _, asset := range release.Assets {
		if asset.Name != name {
			continue
		}
		if !strings.HasPrefix(asset.URL, "https://github.com/roadrunner-server/roadrunner/releases/download/") {
			return fmt.Errorf("unexpected RoadRunner asset URL")
		}
		archive, err := fetch(asset.URL, 100<<20)
		if err != nil {
			return err
		}
		binary, err := verifiedRoadRunner(archive, asset.Digest)
		if err != nil {
			return err
		}
		binaryPath := "/opt/roadrunner/" + version + "/rr"
		if err := h.write(binaryPath, binary, 0755); err != nil {
			return err
		}
		if err := os.MkdirAll(h.path("/usr/local/bin"), 0755); err != nil {
			return err
		}
		link := h.path("/usr/local/bin/rr")
		if info, err := os.Lstat(link); err == nil && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("%s is not a symlink; relocate the existing binary before setup", link)
		}
		tmpDir, err := os.MkdirTemp(filepath.Dir(link), ".abr-rr-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmpDir)
		tmp := filepath.Join(tmpDir, "rr")
		if err := os.Symlink(binaryPath, tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, link); err != nil {
			return err
		}
		return h.command("/usr/local/bin/rr", "--version")
	}
	return fmt.Errorf("release has no %s asset", name)
}

// Extract only the rr executable, never arbitrary paths or symlinks from an archive.
func verifiedRoadRunner(archive []byte, digest string) ([]byte, error) {
	sum := sha256.Sum256(archive)
	if digest != "sha256:"+hex.EncodeToString(sum[:]) {
		return nil, fmt.Errorf("RoadRunner SHA256 missing or mismatched")
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	r := tar.NewReader(gz)
	var binary []byte
	for {
		entry, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if filepath.Base(entry.Name) != "rr" {
			continue
		}
		if binary != nil || entry.Typeflag != tar.TypeReg || entry.Size <= 0 || entry.Size > 200<<20 {
			return nil, fmt.Errorf("invalid RoadRunner executable entry")
		}
		binary, err = io.ReadAll(io.LimitReader(r, 200<<20))
		if err != nil {
			return nil, err
		}
	}
	if len(binary) < 64 || string(binary[:4]) != "\x7fELF" || binary[4] != 2 || binary[5] != 1 || binary[18] != 62 || binary[19] != 0 {
		return nil, fmt.Errorf("RoadRunner archive has no Linux AMD64 ELF executable")
	}
	return binary, nil
}

func (h Host) installTemplates(source fs.FS) error {
	if h.DryRun {
		h.say("Would install embedded templates into %s, preserving edits", h.TemplatesDir)
		return nil
	}
	entries, err := fs.ReadDir(source, ".")
	if err != nil {
		return fmt.Errorf("read embedded templates: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".tmpl") {
			continue
		}
		target := filepath.Join(h.TemplatesDir, entry.Name())
		if _, err := h.read(target); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		data, err := fs.ReadFile(source, entry.Name())
		if err != nil {
			return err
		}
		if err := h.write(target, data, 0644); err != nil {
			return err
		}
	}
	return nil
}
