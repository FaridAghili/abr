// Package disk formats portable disk usage reports. Host measurements live in host.
package disk

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"abr/internal/table"
)

type App struct {
	Name            string `json:"name"`
	ProjectBytes    uint64 `json:"project_bytes"`
	HomeBytes       uint64 `json:"home_bytes"`
	DeploymentBytes uint64 `json:"deployment_bytes"`
	FilesBytes      uint64 `json:"files_bytes"`
	DatabaseBytes   uint64 `json:"database_estimated_bytes"`
	DatabaseManaged bool   `json:"database_managed"`
	TotalBytes      uint64 `json:"total_estimated_bytes"`
}

type Filesystem struct {
	Path           string  `json:"path"`
	CapacityBytes  uint64  `json:"capacity_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsedPercent    float64 `json:"used_percent"`
}

type Report struct {
	MeasuredAt           time.Time    `json:"measured_at,omitzero"`
	FilesystemMeasuredAt time.Time    `json:"filesystem_measured_at,omitzero"`
	FilesystemOnly       bool         `json:"filesystem_only"`
	Cached               bool         `json:"cached"`
	Apps                 []App        `json:"apps"`
	FilesBytes           uint64       `json:"files_bytes"`
	DatabaseBytes        uint64       `json:"database_estimated_bytes"`
	TotalBytes           uint64       `json:"total_estimated_bytes"`
	Filesystems          []Filesystem `json:"filesystems"`
	Notes                []string     `json:"notes"`
}

func Bytes(n uint64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	i := 0
	for value >= 1024 && i < len(units)-1 {
		value /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", value, units[i])
}

func Write(w io.Writer, r Report, jsonOutput bool) error {
	if w == nil {
		w = io.Discard
	}
	if jsonOutput {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(r)
	}
	if !r.MeasuredAt.IsZero() {
		label := "fresh scan"
		if r.Cached {
			label = "cached; --refresh rescans"
		}
		apps := table.Table{
			Title:   fmt.Sprintf("App sizes measured %s (%s)", r.MeasuredAt.Local().Format(time.RFC3339), label),
			Headers: []string{"APP", "PROJECT", "HOME/CACHE", "DEPLOY", "DATABASE~", "TOTAL~"},
			Right:   []int{1, 2, 3, 4, 5},
		}
		for _, a := range r.Apps {
			database := table.Styled("—", table.Muted)
			if a.DatabaseManaged {
				database = table.Text(Bytes(a.DatabaseBytes))
			}
			apps.Rows = append(apps.Rows, []table.Cell{table.Styled(a.Name, table.Strong), table.Text(Bytes(a.ProjectBytes)), table.Text(Bytes(a.HomeBytes)), table.Text(Bytes(a.DeploymentBytes)), database, table.Text(Bytes(a.TotalBytes))})
		}
		apps.Rows = append(apps.Rows, []table.Cell{table.Styled("TOTAL", table.Strong), {}, {}, {}, table.Styled(Bytes(r.DatabaseBytes), table.Strong), table.Styled(Bytes(r.TotalBytes), table.Strong)})
		if err := table.Write(w, apps); err != nil {
			return err
		}
		fmt.Fprintf(w, "App files: %s; databases (estimate): %s; combined (estimate): %s\n\n", Bytes(r.FilesBytes), Bytes(r.DatabaseBytes), Bytes(r.TotalBytes))
	}
	filesystems := table.Table{Headers: []string{"FILESYSTEM PATH", "CAPACITY", "USED", "AVAILABLE", "USE%"}, Right: []int{1, 2, 3, 4}}
	for _, f := range r.Filesystems {
		use := table.Styled(fmt.Sprintf("%.1f%%", f.UsedPercent), table.Good)
		if f.UsedPercent >= 90 {
			use.Tone = table.Bad
		} else if f.UsedPercent >= 75 {
			use.Tone = table.Warn
		}
		filesystems.Rows = append(filesystems.Rows, []table.Cell{table.Styled(f.Path, table.Strong), table.Text(Bytes(f.CapacityBytes)), table.Text(Bytes(f.UsedBytes)), table.Text(Bytes(f.AvailableBytes)), use})
	}
	if err := table.Write(w, filesystems); err != nil {
		return err
	}
	for _, note := range r.Notes {
		fmt.Fprintln(w, note)
	}
	return nil
}

// ParseDU accepts GNU du's NUL-delimited, allocated-byte summaries. Requiring
// every requested path prevents an incomplete scan being shown as zero usage.
func ParseDU(output []byte, paths []string) (map[string]uint64, error) {
	expected := make(map[string]bool, len(paths))
	for _, path := range paths {
		expected[path] = true
	}
	sizes := make(map[string]uint64, len(paths))
	for _, record := range strings.Split(string(output), "\x00") {
		if record == "" {
			continue
		}
		number, path, ok := strings.Cut(record, "\t")
		if !ok || !expected[path] {
			return nil, fmt.Errorf("unexpected disk usage output")
		}
		if _, duplicate := sizes[path]; duplicate {
			return nil, fmt.Errorf("duplicate disk usage summary")
		}
		value, err := strconv.ParseUint(number, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid disk usage byte count")
		}
		sizes[path] = value
	}
	if len(sizes) != len(expected) {
		return nil, fmt.Errorf("incomplete disk usage output")
	}
	return sizes, nil
}
