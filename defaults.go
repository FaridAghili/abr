// Package abr provides the default templates and example configuration bundled
// with the executable. Installed templates remain editable on the host.
package abr

import (
	"embed"
	"io/fs"
)

// Version is shared by the CLI, TUI and outbound download requests.
const Version = "1.0.0"

//go:embed templates/*.tmpl config.example.toml
var defaults embed.FS

// TemplateFS returns the bundled runtime templates, with filenames at its root.
func TemplateFS() (fs.FS, error) { return fs.Sub(defaults, "templates") }

// ExampleConfig returns the generic example, never the host's live configuration.
func ExampleConfig() ([]byte, error) { return defaults.ReadFile("config.example.toml") }
