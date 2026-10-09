// Package disk formats portable disk usage reports. Host measurements live in host.
package disk

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
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
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if !r.MeasuredAt.IsZero() {
		label := "fresh scan"
		if r.Cached {
			label = "cached; --refresh rescans"
		}
		fmt.Fprintf(table, "App sizes measured %s (%s)\n", r.MeasuredAt.Local().Format(time.RFC3339), label)
		fmt.Fprintln(table, "APP\tPROJECT\tHOME/CACHE\tDEPLOY\tDATABASE~\tTOTAL~")
		for _, a := range r.Apps {
			database := "—"
			if a.DatabaseManaged {
				database = Bytes(a.DatabaseBytes)
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n", a.Name, Bytes(a.ProjectBytes), Bytes(a.HomeBytes), Bytes(a.DeploymentBytes), database, Bytes(a.TotalBytes))
		}
		fmt.Fprintf(table, "TOTAL\t\t\t\t%s\t%s\n", Bytes(r.DatabaseBytes), Bytes(r.TotalBytes))
		fmt.Fprintf(table, "App files: %s; databases (estimate): %s; combined (estimate): %s\n\n", Bytes(r.FilesBytes), Bytes(r.DatabaseBytes), Bytes(r.TotalBytes))
	}
	fmt.Fprintln(table, "FILESYSTEM PATH\tCAPACITY\tUSED\tAVAILABLE\tUSE%")
	for _, f := range r.Filesystems {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%.1f%%\n", f.Path, Bytes(f.CapacityBytes), Bytes(f.UsedBytes), Bytes(f.AvailableBytes), f.UsedPercent)
	}
	for _, note := range r.Notes {
		fmt.Fprintln(table, note)
	}
	return table.Flush()
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
