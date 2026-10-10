package backup

import (
	"fmt"
	"net"
	"path"
	"regexp"
	"strings"
)

// Destination is stored privately, outside the editable example configuration.
type Destination struct {
	Host      string `json:"host"`
	User      string `json:"user"`
	Directory string `json:"directory"`
	Port      int    `json:"port"`
	Auth      string `json:"auth"`
	Password  string `json:"password,omitempty"`
}

func (d Destination) Validate() error {
	if net.ParseIP(d.Host) == nil && !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]*$`).MatchString(d.Host) {
		return fmt.Errorf("use a server IP address or hostname")
	}
	if !regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`).MatchString(d.User) {
		return fmt.Errorf("invalid backup SSH username")
	}
	if d.Port < 1 || d.Port > 65535 {
		return fmt.Errorf("SSH port must be between 1 and 65535")
	}
	if !path.IsAbs(d.Directory) || path.Clean(d.Directory) != d.Directory || d.Directory == "/" || strings.ContainsAny(d.Directory, "\x00\r\n") {
		return fmt.Errorf("use a clean absolute backup directory outside /")
	}
	switch d.Auth {
	case "key":
		if d.Password != "" {
			return fmt.Errorf("key authentication must not contain a password")
		}
	case "password":
		if d.Password == "" || len(d.Password) > 4096 || strings.ContainsAny(d.Password, "\x00\r\n") {
			return fmt.Errorf("provide a single-line SSH password")
		}
	default:
		return fmt.Errorf("choose key or password authentication")
	}
	return nil
}
