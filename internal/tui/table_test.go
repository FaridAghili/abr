package tui

import (
	"io"
	"strings"
	"testing"

	"abr/internal/table"
)

func TestCommandTablesAreDrawnInThePalette(t *testing.T) {
	o := testOptions(t)
	o.RunCommand = func(args []string, out io.Writer) error {
		io.WriteString(out, "Checking")
		if err := table.Write(out, table.Table{Headers: []string{"APP", "RESULT"}, Rows: [][]table.Cell{
			{table.Styled("shop", table.Strong), table.Styled("ok", table.Good)},
			{table.Styled("blog\x1b[2J", table.Strong), table.Styled("failed", table.Bad)},
		}}); err != nil {
			return err
		}
		io.WriteString(out, "Done\n")
		return nil
	}
	m := newModel(o)
	m.start(action{title: "Backup status", args: []string{"backup", "status"}})
	finishLogView(t, m)
	if strings.Contains(m.output, "\x1b[2J") || !strings.Contains(m.output, "\x1b[") {
		t.Fatal("table not colored, or a cell's terminal control survived", m.output)
	}
	if !strings.Contains(m.output, "Checking\n") {
		t.Fatal("table did not start on its own line")
	}
	view := viewText(m)
	for _, want := range []string{"│ APP  │ RESULT │", "│ shop │ ok     │", "╰", "Done"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q in view:\n%s", want, view)
		}
	}
}
