package theme

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestFitKeepsOneRow(t *testing.T) {
	got := Fit("bash -lc set -u\nexport PATH=x\r\ncd $W", 20)
	if strings.ContainsAny(got, "\r\n") || ansi.StringWidth(got) != 20 {
		t.Fatalf("Fit = %q (width %d)", got, ansi.StringWidth(got))
	}
	if !strings.HasPrefix(got, "bash -lc set -u⏎") {
		t.Fatalf("Fit = %q", got)
	}
}

func TestBoxWithMultilineContentKeepsItsHeight(t *testing.T) {
	b := New(false, true).Box([]string{"a\nb\nc", "d"}, 12, 5, BoxOpts{Title: "x"})
	if len(b) != 5 {
		t.Fatalf("%d rows", len(b))
	}
	for _, l := range b {
		if strings.Contains(l, "\n") || ansi.StringWidth(l) != 12 {
			t.Fatalf("row %q", l)
		}
	}
}
