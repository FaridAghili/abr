package host

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"abr/internal/config"
)

func (h Host) serverHostname() (string, error) {
	if err := h.trustedFile(h.path("/etc/hostname")); err != nil {
		return "", fmt.Errorf("read VPS name; run abr setup first: %w", err)
	}
	data, err := h.read("/etc/hostname")
	if err != nil {
		return "", fmt.Errorf("read VPS name; run abr setup first: %w", err)
	}
	name := strings.TrimSpace(string(data))
	if err := config.ValidateHostname(name); err != nil {
		return "", fmt.Errorf("set the VPS name with abr setup first: %w", err)
	}
	return name, nil
}

func hostnameHosts(data []byte, name string) []byte {
	lines := []string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		content, comment, hasComment := strings.Cut(line, "#")
		fields := strings.Fields(content)
		if len(fields) > 0 && fields[0] == "127.0.1.1" {
			if hasComment {
				lines = append(lines, "#"+comment)
			}
			continue
		}
		lines = append(lines, line)
	}
	return []byte(strings.Join(lines, "\n") + "\n127.0.1.1\t" + name + "\n")
}

func (h Host) configureHostname(name string) error {
	if err := config.ValidateHostname(name); err != nil {
		return err
	}
	if !h.DryRun {
		for _, path := range []string{"/etc/hostname", "/etc/hosts"} {
			if err := h.trustedFile(h.path(path)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return h.setupConfig("hostname.cfg.tmpl", "/etc/cloud/cloud.cfg.d/99-abr-hostname.cfg", func() error {
		if h.DryRun {
			h.say("Would map %s to 127.0.1.1 in /etc/hosts", name)
			return h.command("hostnamectl", "hostname", name)
		}
		old, err := h.read("/etc/hosts")
		existed := err == nil
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := h.write("/etc/hosts", hostnameHosts(old, name), 0644); err != nil {
			return err
		}
		if err := h.command("hostnamectl", "hostname", name); err != nil {
			if existed {
				return errors.Join(err, h.write("/etc/hosts", old, 0644))
			}
			return errors.Join(err, h.removeFile("/etc/hosts"))
		}
		actual, err := h.serverHostname()
		if err != nil {
			return err
		}
		if actual != name {
			return fmt.Errorf("Ubuntu hostname did not become %s", name)
		}
		h.say("VPS name set to %s", name)
		return nil
	})
}
