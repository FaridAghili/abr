package host

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"sites-manager/internal/services"
)

func (h Host) installComposer() error {
	h.say("Install latest stable Composer with official SHA256 verification")
	if h.DryRun {
		return nil
	}
	metadata, err := fetch("https://getcomposer.org/versions", 1<<20)
	if err != nil {
		return err
	}
	var versions struct {
		Stable []struct {
			Version string `json:"version"`
			Path    string `json:"path"`
			MinPHP  int    `json:"min-php"`
		} `json:"stable"`
	}
	if err := json.Unmarshal(metadata, &versions); err != nil {
		return err
	}
	if len(versions.Stable) == 0 {
		return fmt.Errorf("Composer has no stable release")
	}
	v := versions.Stable[0]
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(v.Version) || v.Path != "/download/"+v.Version+"/composer.phar" || v.MinPHP > 80500 {
		return fmt.Errorf("unsupported Composer release metadata")
	}
	url := "https://getcomposer.org" + v.Path
	digest, err := fetch(url+".sha256sum", 1024)
	if err != nil {
		return err
	}
	phar, err := fetch(url, 16<<20)
	if err != nil {
		return err
	}
	if err := verifySHA256(phar, string(digest)); err != nil {
		return fmt.Errorf("Composer: %w", err)
	}
	if err := h.write("/usr/local/bin/composer", phar, 0755); err != nil {
		return err
	}
	return h.command("/usr/local/bin/composer", "--no-plugins", "--no-scripts", "--version", "--no-ansi")
}

func verifySHA256(data []byte, digest string) error {
	fields := strings.Fields(digest)
	sum := sha256.Sum256(data)
	if len(fields) == 0 || fields[0] != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("SHA256 missing or mismatched")
	}
	return nil
}

func (h Host) installNPM(images bool) error {
	out, err := h.run("Verify Node 24", Command{Name: "/usr/bin/node", Args: []string{"-p", "process.versions.node"}, Private: true})
	if err != nil {
		return err
	}
	if !h.DryRun && !strings.HasPrefix(strings.TrimSpace(string(out)), "24.") {
		return fmt.Errorf("Node 24 is required")
	}
	if err := h.command("/usr/bin/npm", "install", "--global", "--prefix", "/usr/local", "--ignore-scripts", "--engine-strict", "npm@latest"); err != nil {
		return err
	}
	packages := []string{"install", "--global", "--prefix", "/usr/local", "--ignore-scripts", "--engine-strict", "npm-check-updates@latest"}
	if images {
		packages = append(packages, "svgo@latest")
	}
	if err := h.command("/usr/local/bin/npm", packages...); err != nil {
		return err
	}
	return h.command("/usr/local/bin/npm", "--version")
}

func (h Host) configureRedis() (result error) {
	const include = "include /etc/redis/sites.conf"
	path := "/etc/redis/redis.conf"
	old, err := h.read(path)
	if err != nil && !h.DryRun {
		return err
	}
	defer func() {
		if result != nil && !h.DryRun {
			result = errors.Join(result, h.write(path, old, 0640), h.command("chown", "root:redis", path), h.command("systemctl", "restart", "redis-server"))
		}
	}()
	return h.setupConfig("redis-hardening.conf.tmpl", "/etc/redis/sites.conf", func() error {
		if !hasDirective(old, include) {
			if err := h.write(path, append(append([]byte(nil), old...), []byte("\n# Sites local Redis configuration\n"+include+"\n")...), 0640); err != nil {
				return err
			}
			// The packaged config belongs to redis; atomic replacement creates a
			// root-owned file, so restore its service-readable group explicitly.
			if err := h.command("chown", "root:redis", path); err != nil {
				return err
			}
		}
		// Prevent the redis service account from replacing root-managed config.
		if err := h.command("chown", "root:redis", "/etc/redis"); err != nil {
			return err
		}
		if err := h.command("chmod", "0750", "/etc/redis"); err != nil {
			return err
		}
		if err := h.command("systemctl", "enable", "--now", "redis-server"); err != nil {
			return err
		}
		if err := h.command("systemctl", "restart", "redis-server"); err != nil {
			return err
		}
		out, err := h.run("Verify local protected Redis", Command{Name: "redis-cli", Args: []string{"CONFIG", "GET", "bind", "protected-mode"}, Private: true})
		if err != nil {
			return err
		}
		if !h.DryRun && (!strings.Contains(string(out), "127.0.0.1 -::1") || !strings.Contains(string(out), "yes")) {
			return fmt.Errorf("Redis loopback/protected-mode settings could not be verified")
		}
		return nil
	})
}

// Test an actual SVG-to-PNG conversion, rather than merely listing a codec.
// Keep ImageMagick's packaged limits and disabled unsafe coders intact.
func (h Host) verifyPHP() error {
	code := `if (PHP_MAJOR_VERSION !== 8 || PHP_MINOR_VERSION !== 5) { fwrite(STDERR, "PHP 8.5 required\n"); exit(1); }
foreach (["bcmath","curl","gd","imagick","intl","mbstring","pdo_mysql","redis","xml","zip","pcntl","excimer"] as $ext) {
    if (!extension_loaded($ext)) { fwrite(STDERR, "Missing extension: $ext\n"); exit(1); }
}
$image = new Imagick();
$image->readImageBlob('<svg xmlns="http://www.w3.org/2000/svg" width="8" height="8"><rect width="8" height="8" fill="red"/></svg>');
$image->setImageFormat("png");
if (!str_starts_with($image->getImageBlob(), "\x89PNG\r\n\x1a\n")) { exit(1); }
echo "PHP 8.5 extensions and Imagick SVG conversion verified\n";`
	_, err := h.run("Verify PHP extensions and Imagick SVG conversion", Command{Name: "/usr/bin/php" + services.PHPVersion, Args: []string{"-r", code}})
	return err
}

func (h Host) configurePHP() error {
	base := "/etc/php/" + services.PHPVersion
	if err := h.setupConfig("php-cli.ini.tmpl", filepath.Join(base, "cli/conf.d/99-sites.ini"), h.verifyPHP); err != nil {
		return err
	}
	return h.setupConfig("php-fpm.ini.tmpl", filepath.Join(base, "fpm/conf.d/99-sites.ini"), func() error {
		if err := h.command("/usr/sbin/php-fpm"+services.PHPVersion, "--test"); err != nil {
			return err
		}
		return h.command("systemctl", "reload", "php"+services.PHPVersion+"-fpm")
	})
}

func requireSetupTemplates(source string) error {
	for _, name := range []string{"caddy-site.caddy.tmpl", "nuxt.service.tmpl", "octane.service.tmpl", "php-fpm-pool.conf.tmpl", "queue-worker.service.tmpl", "scheduler.service.tmpl", "scheduler.timer.tmpl", "nightwatch.service.tmpl", "inertia-ssr.service.tmpl", "php-cli.ini.tmpl", "php-fpm.ini.tmpl", "ssh-hardening.conf.tmpl", "mysql-hardening.cnf.tmpl", "redis-hardening.conf.tmpl", "automatic-updates.conf.tmpl"} {
		if info, err := os.Stat(filepath.Join(source, name)); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("missing distribution template %s; keep templates/ beside the executable", name)
		}
	}
	return nil
}

// Ignore commented imports and normalize spacing when recognizing directives.
func hasDirective(data []byte, directive string) bool {
	for _, line := range strings.Split(string(data), "\n") {
		line, _, _ = strings.Cut(line, "#")
		if strings.Join(strings.Fields(line), " ") == directive {
			return true
		}
	}
	return false
}

func (h Host) configureCaddyImport() error {
	const path = "/etc/caddy/Caddyfile"
	const directive = "import /etc/caddy/sites.d/sites-*.caddy"
	old, err := h.read(path)
	if err != nil && !h.DryRun {
		return err
	}
	changed := !hasDirective(old, directive)
	if changed {
		data := append(append([]byte(nil), old...), []byte("\n# Sites application configuration\n"+directive+"\n")...)
		if err := h.write(path, data, 0644); err != nil {
			return err
		}
	}
	if err := h.command("caddy", "validate", "--config", path, "--adapter", "caddyfile"); err != nil {
		if changed && !h.DryRun {
			return errors.Join(err, h.write(path, old, 0644))
		}
		return err
	}
	return nil
}
