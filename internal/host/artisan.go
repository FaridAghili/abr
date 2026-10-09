package host

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"abr/internal/services"
)

// LaravelProcess prepares a foreground Artisan command or interactive app shell.
// Account checks run under the host lock; the foreground session does not hold it.
func (h Host) LaravelProcess(name string, args []string, shell bool) (*exec.Cmd, error) {
	var command *exec.Cmd
	err := h.locked(func() error {
		a, registry, err := h.application(name)
		if err != nil {
			return err
		}
		if a.Type != "laravel" {
			return fmt.Errorf("Artisan access is only available for Laravel apps")
		}
		if err := h.project(a); err != nil {
			return err
		}
		plan, err := services.Render(a, registry, h.TemplatesDir, h.Manager.StateDir)
		if err != nil {
			return err
		}
		if h.DryRun {
			h.say("Would open Laravel access for %s as %s in %s; no command executed", a.Name, a.User, a.Directory)
			return nil
		}
		if _, err := h.environmentAccount(a); err != nil {
			return err
		}
		for _, file := range []string{"artisan", "vendor/autoload.php"} {
			info, err := os.Stat(h.path(filepath.Join(a.Directory, file)))
			if err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("%s requires %s; deploy the Laravel app first", a.Name, file)
			}
		}
		php := "/usr/bin/php" + services.PHPVersion
		if shell {
			plan.Environment["HISTFILE"] = "/dev/null"
			// Fixed code only: no app name or user-supplied command is interpolated.
			// Export helpers to a shell without startup files or persistent history.
			script := `artisan() { ` + php + ` artisan "$@"; }; php() { ` + php + ` "$@"; }; export -f artisan php
printf 'Run artisan cache:clear, artisan db:seed, or your admin command. Type exit to return.\n'
exec /bin/bash --noprofile --norc -i`
			command = appProcess(a, plan.Environment, "/bin/bash", "--noprofile", "--norc", "-c", script)
		} else {
			command = appProcess(a, plan.Environment, php, append([]string{"artisan"}, args...)...)
		}
		command.Dir = h.path(a.Directory)
		return nil
	})
	return command, err
}
