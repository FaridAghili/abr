package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Update refreshes shared host packages and tools; application deployments use
// their own commands and lockfiles.
func (h Host) Update() error {
	return h.locked(func() error {
		if !h.DryRun {
			if _, err := h.nodeToolsDirectory(); err != nil {
				return fmt.Errorf("run abr setup before updating the server: %w", err)
			}
			if err := h.trustedDirectory(h.path("/usr/local/bin")); err != nil {
				return err
			}
			if err := h.trustedFile(h.path("/usr/local/bin/composer")); err != nil {
				return fmt.Errorf("Composer installation: %w", err)
			}
		}
		for _, operation := range [][]string{
			{"-o", "APT::Update::Error-Mode=any", "update"},
			{"-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold", "full-upgrade", "-y"},
			{"autoremove", "-y"},
			{"autoclean", "-y"},
		} {
			args := append([]string{"-o", "DPkg::Lock::Timeout=120"}, operation...)
			if _, err := h.run("apt-get "+strings.Join(operation, " "), Command{Name: "apt-get", Args: args, Env: []string{"DEBIAN_FRONTEND=noninteractive"}, Stream: true}); err != nil {
				return err
			}
		}
		// Keep self-update's keys and backups separate from app repository auth.
		home := filepath.Join(h.Manager.StateDir, "composer-update")
		if !h.DryRun {
			if err := h.trustedAncestor(h.path(home)); err != nil {
				return err
			}
			if err := os.MkdirAll(h.path(home), 0700); err != nil {
				return err
			}
			if err := h.trustedDirectory(h.path(home)); err != nil {
				return err
			}
		}
		if _, err := h.run("composer self-update --stable --no-interaction", Command{
			Name: "/usr/local/bin/composer", Dir: "/",
			Args: []string{"--no-plugins", "--no-scripts", "--no-interaction", "self-update", "--stable"},
			Env:  []string{"COMPOSER_ALLOW_SUPERUSER=1", "COMPOSER_HOME=" + home}, Stream: true,
		}); err != nil {
			return err
		}
		if err := h.verifyNode24(); err != nil {
			return err
		}
		if err := h.updateNodeTools(); err != nil {
			return err
		}
		if h.DryRun {
			h.say("Server update preview complete; no commands executed or files changed")
		} else {
			h.say("Server update complete; apt packages, Composer and global npm tools updated")
		}
		return nil
	})
}
