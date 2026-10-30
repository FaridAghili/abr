package host

import (
	"fmt"
	"testing"

	"abr/internal/table"
)

func TestServiceRowsReadSystemctlShow(t *testing.T) {
	states := parseUnitStates("Id=abr-app-queue@1.service\nLoadState=loaded\nActiveState=active\nSubState=running\nUnitFileState=enabled\nActiveEnterTimestamp=Thu 2026-10-29 21:00:01 UTC\nMainPID=4242\nMemoryCurrent=52428800\nNRestarts=2\n\n" +
		"Id=abr-app-scheduler.timer\nLoadState=loaded\nActiveState=active\nSubState=waiting\nUnitFileState=enabled\nActiveEnterTimestamp=Thu 2026-10-29 21:00:01 UTC\nMainPID=0\nMemoryCurrent=[not set]\nNRestarts=\n\n" +
		"Id=abr-app-web.service\nLoadState=loaded\nActiveState=failed\nSubState=failed\nUnitFileState=enabled\nActiveEnterTimestamp=\nMainPID=0\nMemoryCurrent=18446744073709551615\nNRestarts=0\n\n" +
		"Id=php8.5-fpm.service\nLoadState=not-found\nActiveState=inactive\nSubState=dead\n")
	text := func(row []table.Cell) (values []string) {
		for _, cell := range row {
			values = append(values, cell.Text)
		}
		return values
	}
	for unit, want := range map[string][]string{
		"abr-app-queue@1.service": {"queue@1", "running", "enabled", "2026-10-29 21:00", "50.0 MiB", "2"},
		"abr-app-scheduler.timer": {"scheduler.timer", "waiting", "enabled", "2026-10-29 21:00", "—", "—"},
		"abr-app-web.service":     {"web", "failed", "enabled", "—", "—", "0"},
		"php8.5-fpm.service":      {"php8.5-fpm (shared)", "not installed", "—", "—", "—", "—"},
	} {
		row := serviceRow("app", unit, states[unit])
		if got := text(row); len(got) != len(want) || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: got %q, want %q", unit, got, want)
		}
	}
	if row := serviceRow("app", "abr-app-web.service", states["abr-app-web.service"]); row[1].Tone != table.Bad {
		t.Fatal("failed service not marked bad")
	}
	if row := serviceRow("app", "abr-app-queue@1.service", states["abr-app-queue@1.service"]); row[1].Tone != table.Good || row[5].Tone != table.Warn {
		t.Fatal("running service or restarts colored wrongly")
	}
}
