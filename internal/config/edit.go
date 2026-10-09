package config

import (
	"fmt"
	"slices"
)

// AppChanges holds only explicitly supplied settings. Identity and paths cannot
// be edited, and omitted fields retain their value under the manager's lock.
type AppChanges struct {
	Values        App
	Fields        []string
	CanonicalHost string
}

func (changes AppChanges) Apply(a App) (App, error) {
	v := changes.Values
	for _, field := range changes.Fields {
		switch field {
		case "domain":
			a.Domain = v.Domain
		case "alias":
			a.Aliases = append([]string(nil), v.Aliases...)
		case "serving-domain":
			a.Domains = append([]string(nil), v.Domains...)
		case "embed-path":
			a.Embedding.Paths = append([]string(nil), v.Embedding.Paths...)
		case "embed-origin":
			a.Embedding.Origins = append([]string(nil), v.Embedding.Origins...)
		case "health-check":
			a.HealthCheck = v.HealthCheck
		case "build-order":
			a.BuildOrder = v.BuildOrder
		case "web-driver":
			a.Web.Driver = v.Web.Driver
		case "octane-workers":
			a.Web.Workers = v.Web.Workers
		case "queue-workers":
			a.Queue.Workers = v.Queue.Workers
		case "scheduler":
			a.Scheduler.Enabled = v.Scheduler.Enabled
		case "nightwatch":
			a.Nightwatch.Enabled = v.Nightwatch.Enabled
		case "inertia-ssr":
			a.InertiaSSR.Enabled = v.InertiaSSR.Enabled
		case "database":
			a.Database.Enabled = v.Database.Enabled
		case "canonical-host": // Apply after the primary domain and alias changes.
		default:
			return App{}, fmt.Errorf("unsupported app setting %q", field)
		}
	}
	if a.Web.Driver == "fpm" && slices.Contains(changes.Fields, "web-driver") && !slices.Contains(changes.Fields, "octane-workers") {
		a.Web.Workers = 0
	}
	if changes.CanonicalHost != "" {
		var err error
		a, err = a.WithCanonicalHost(changes.CanonicalHost)
		if err != nil {
			return App{}, err
		}
	}
	return a, a.Validate()
}
