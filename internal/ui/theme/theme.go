// Package theme holds the semantic styles shared by the dashboard, the
// wizards and the apply progress output. Views ask for a role (accent, muted,
// ok, warn, err, …) instead of a color number, so the palette lives here.
//
// Colors are the 16 ANSI colors: the user's terminal theme decides the actual
// RGB values, which keeps light and dark backgrounds readable. With color off
// (--color never, NO_COLOR, not a terminal) every style renders plain text and
// no escape sequences; meaning is carried by structural markers (borders, ›,
// ●/○, [x], ▌, brackets) that are drawn either way.
package theme

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Theme is a set of semantic styles.
type Theme struct {
	Color bool

	Text    lipgloss.Style // ordinary content
	Bold    lipgloss.Style // emphasis without color
	Muted   lipgloss.Style // secondary text, hints, keys of key/value pairs
	Accent  lipgloss.Style // focus, titles, the current choice
	Key     lipgloss.Style // a key in a key hint
	OK      lipgloss.Style
	Warn    lipgloss.Style
	Err     lipgloss.Style
	Info    lipgloss.Style
	Pending lipgloss.Style

	Badge       lipgloss.Style // app name / active tab chip
	Sel         lipgloss.Style // background of the selected row in a focused list
	Marked      lipgloss.Style // background of rows marked for copying
	BorderFocus lipgloss.Style
	BorderDim   lipgloss.Style
	ErrBorder   lipgloss.Style
	Button      lipgloss.Style // focused button
	ButtonDim   lipgloss.Style // unfocused button
}

// New returns the default theme; colored=false yields plain styles. dark is
// the terminal background (from tea.BackgroundColorMsg; assume dark until
// the terminal answers).
func New(colored, dark bool) Theme {
	if !colored {
		p := lipgloss.NewStyle()
		return Theme{Text: p, Bold: p, Muted: p, Accent: p, Key: p, OK: p, Warn: p, Err: p, Info: p, Pending: p,
			Badge: p, Sel: p, Marked: p, BorderFocus: p, BorderDim: p, ErrBorder: p, Button: p, ButtonDim: p}
	}
	fg := func(c color.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }
	// Chips use reverse video of an ANSI color: the text takes the terminal's
	// own background color, so it contrasts with the chip in light and dark
	// themes alike (a fixed white or black foreground does not).
	chip := func(c color.Color) lipgloss.Style { return fg(c).Reverse(true).Bold(true) }
	// The selected row keeps its segments' status colors, so its background
	// must be subtle: a tint picked for the background brightness.
	sel := lipgloss.LightDark(dark)(lipgloss.Color("#d4e2fb"), lipgloss.Color("#2c3a56"))
	return Theme{
		Color:       true,
		Text:        lipgloss.NewStyle(),
		Bold:        lipgloss.NewStyle().Bold(true),
		Muted:       fg(lipgloss.BrightBlack),
		Accent:      fg(lipgloss.Blue).Bold(true),
		Key:         fg(lipgloss.Blue).Bold(true),
		OK:          fg(lipgloss.Green),
		Warn:        fg(lipgloss.Yellow),
		Err:         fg(lipgloss.Red),
		Info:        fg(lipgloss.Cyan),
		Pending:     fg(lipgloss.Yellow),
		Badge:       chip(lipgloss.Blue),
		Sel:         lipgloss.NewStyle().Background(sel),
		Marked:      chip(lipgloss.Magenta).Bold(false),
		BorderFocus: fg(lipgloss.Blue).Bold(true),
		BorderDim:   fg(lipgloss.BrightBlack),
		ErrBorder:   fg(lipgloss.Red).Bold(true),
		Button:      chip(lipgloss.Blue),
		ButtonDim:   fg(lipgloss.BrightBlack),
	}
}

// Status returns the style for a status word: ok, warn, err, info, pending,
// skip (anything else is plain).
func (t Theme) Status(kind string) lipgloss.Style {
	switch kind {
	case "ok", "done", "running", "online":
		return t.OK
	case "warn", "pending", "killed":
		return t.Warn
	case "err", "fail", "failed", "lost", "offline":
		return t.Err
	case "info":
		return t.Info
	case "skip", "muted":
		return t.Muted
	}
	return t.Text
}

// Mark is the one-cell symbol for a check status.
func (t Theme) Mark(status string) string {
	switch status {
	case "ok":
		return t.OK.Render("✓")
	case "warn":
		return t.Warn.Render("!")
	case "skip":
		return t.Muted.Render("-")
	case "info":
		return t.Info.Render("i")
	default:
		return t.Err.Render("✗")
	}
}

// KeyHint renders "key label" with the key emphasized.
func (t Theme) KeyHint(key, label string) string {
	return t.Key.Render(key) + " " + t.Muted.Render(label)
}

// Meter is a bar of width cells for pct (0–100), colored by threshold.
func (t Theme) Meter(pct float64, width int) string {
	n := int(pct/100*float64(width) + 0.5)
	n = max(0, min(n, width))
	st := t.OK
	switch {
	case pct >= 90:
		st = t.Err
	case pct >= 70:
		st = t.Warn
	}
	return st.Render(strings.Repeat("█", n)) + t.Muted.Render(strings.Repeat("░", width-n))
}

// ButtonText renders a button. The focused one carries brackets so focus is
// visible without color; both have the same width so focus never shifts the
// layout.
func (t Theme) ButtonText(label string, focused bool) string {
	if focused {
		return t.Button.Render("[ " + label + " ]")
	}
	return t.ButtonDim.Render("  " + label + "  ")
}

// PrimaryText renders the main action of a screen: a chip when focused,
// accent text otherwise, so it stands out before it has focus.
func (t Theme) PrimaryText(label string, focused bool) string {
	if focused {
		return t.Button.Render("[ " + label + " ]")
	}
	return t.Accent.Render("  " + label + "  ")
}

// ButtonWidth is the cell width of ButtonText(label, …).
func ButtonWidth(label string) int { return ansi.StringWidth(label) + 4 }

// Fit truncates s to w cells and pads it with spaces to exactly w cells;
// embedded newlines become ⏎ so the result is always one row.
func Fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if strings.ContainsAny(s, "\r\n") { // one screen row: an embedded newline would break the box
		s = strings.NewReplacer("\r\n", "⏎", "\n", "⏎", "\r", "⏎").Replace(s)
	}
	s = ansi.Truncate(s, w, "…")
	if pad := w - ansi.StringWidth(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// Rect is a cell rectangle.
type Rect struct{ X, Y, W, H int }

// Has reports whether the cell (x, y) is inside r.
func (r Rect) Has(x, y int) bool { return x >= r.X && x < r.X+r.W && y >= r.Y && y < r.Y+r.H }

// Inner is the content area of a Box drawn in r (border plus one space of
// horizontal padding).
func (r Rect) Inner() Rect {
	return Rect{X: r.X + 2, Y: r.Y + 1, W: max(0, r.W-4), H: max(0, r.H-2)}
}

// BoxOpts decorate a box.
type BoxOpts struct {
	Title   string // inset in the top border
	Footer  string // inset at the right of the bottom border (e.g. "↓ more")
	Focused bool
	Danger  bool // red border (destructive confirmations)
}

// Box draws lines inside a rounded border exactly w×h cells. Content lines are
// truncated/padded to the inner width (w-4) and the first h-2 are used.
func (t Theme) Box(lines []string, w, h int, o BoxOpts) []string {
	if w < 2 || h < 2 {
		return fitLines(nil, h, w)
	}
	border, title := t.BorderDim, t.Muted
	if o.Focused {
		border, title = t.BorderFocus, t.Accent
	}
	if o.Danger {
		border, title = t.ErrBorder, t.Err.Bold(true)
	}
	inner := max(0, w-4)
	top := border.Render("╭─")
	used := 2
	if o.Title != "" && w > 6 {
		tt := ansi.Truncate(" "+o.Title+" ", w-4, "…")
		top += title.Render(tt)
		used += ansi.StringWidth(tt)
	}
	top += border.Render(strings.Repeat("─", max(0, w-used-1)) + "╮")
	bottom := "╰"
	used = 1
	foot := ""
	if o.Footer != "" && w > 8 {
		foot = ansi.Truncate(" "+o.Footer+" ", w-4, "…")
	}
	fill := max(0, w-used-1-ansi.StringWidth(foot)-1)
	bottom = border.Render(bottom+strings.Repeat("─", fill)) + t.Muted.Render(foot) + border.Render("─╯")
	if foot == "" {
		bottom = border.Render("╰" + strings.Repeat("─", max(0, w-2)) + "╯")
	}
	out := []string{top}
	side := border.Render("│")
	for i := 0; i < h-2; i++ {
		l := ""
		if i < len(lines) {
			l = lines[i]
		}
		out = append(out, side+" "+Fit(l, inner)+" "+side)
	}
	return append(out, bottom)
}

func fitLines(lines []string, n, w int) []string {
	out := make([]string, max(0, n))
	for i := range out {
		if i < len(lines) {
			out[i] = Fit(lines[i], w)
		} else {
			out[i] = strings.Repeat(" ", max(0, w))
		}
	}
	return out
}

// Join places blocks of equal height side by side.
func Join(blocks ...[]string) []string {
	h := 0
	for _, b := range blocks {
		h = max(h, len(b))
	}
	out := make([]string, h)
	for _, b := range blocks {
		for i := 0; i < h; i++ {
			if i < len(b) {
				out[i] += b[i]
			}
		}
	}
	return out
}

// Overlay draws a modal box over base at (x, y). The base is dimmed (its
// colors removed and rendered muted) so the modal is clearly in front.
func (t Theme) Overlay(base, box []string, x, y int) []string {
	out := make([]string, len(base))
	for i, b := range base {
		out[i] = t.Muted.Render(ansi.Strip(b))
	}
	for i, l := range box {
		row := y + i
		if row < 0 || row >= len(out) {
			continue
		}
		b := ansi.Strip(base[row])
		bw, lw := ansi.StringWidth(b), ansi.StringWidth(l)
		left := ansi.Cut(b, 0, x)
		if pad := x - ansi.StringWidth(left); pad > 0 {
			left += strings.Repeat(" ", pad)
		}
		right := ""
		if x+lw < bw {
			right = ansi.Cut(b, x+lw, bw)
		}
		out[row] = t.Muted.Render(left) + l + t.Muted.Render(right)
	}
	return out
}
