package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/fleet"
	"github.com/daviddwlee84/lazylongarm/internal/ui/theme"
)

// ---- layout: one pure calculation for rendering and mouse hit-testing ----

type zoneKind int

const (
	zNone   zoneKind = iota
	zTab             // i = tab index
	zList            // the list pane (or the hub info pane)
	zRow             // id = stable row id
	zDetail          // the right/lower pane
	zAction          // id = action id in the footer
	zHelp            // "? help" in the footer
	zModal           // inside the modal box
	zButton          // i = modal button index
)

type zone struct {
	r    theme.Rect
	kind zoneKind
	i    int
	id   string
}

type footItem struct {
	key, label string
	kind       zoneKind
	id         string
}

type layout struct {
	w, h    int
	stacked bool

	list, detail theme.Rect // outer pane rects; W == 0 when hidden
	ids          []string   // list ids in display order
	listOff      int        // first visible list item
	listRows     int        // visible item rows

	dLines []string // full detail content
	dPin   int      // leading lines that stay put while the rest scrolls
	dOff   int      // first visible scrolled line (after the pinned ones)
	dMax   int      // largest dOff

	head, tail []footItem // footer items that fit (left / right-aligned)
	tailX      int

	modal      theme.Rect // zero unless an overlay is open
	modalLines []string   // help body
	helpOff    int
	buttons    []string // confirm buttons, in order
	zones      []zone
}

// hit returns the innermost zone at (x, y); later zones win.
func (l layout) hit(x, y int) zone {
	for i := len(l.zones) - 1; i >= 0; i-- {
		if l.zones[i].r.Has(x, y) {
			return l.zones[i]
		}
	}
	return zone{}
}

// detailRows are the content line indexes shown in the detail pane, top to
// bottom: the pinned lines, then the scrolled window.
func (l layout) detailRows() []int {
	in := l.detail.Inner()
	var rows []int
	for i := 0; i < l.dPin && i < in.H && i < len(l.dLines); i++ {
		rows = append(rows, i)
	}
	for i := l.dPin + l.dOff; i < len(l.dLines) && len(rows) < in.H; i++ {
		rows = append(rows, i)
	}
	return rows
}

// detailLineAt maps a screen row of the detail pane to a content line (rows
// above or below the pane clamp to its first or last visible line).
func (l layout) detailLineAt(y int) int {
	rows := l.detailRows()
	if len(rows) == 0 {
		return 0
	}
	return rows[max(0, min(y-l.detail.Inner().Y, len(rows)-1))]
}

func (m *Model) layout() layout {
	w, h := m.width, m.height
	l := layout{w: w, h: h}
	bodyY, bodyH := 2, max(1, h-4)

	x := 1
	for i := range tabNames {
		lw := ansi.StringWidth(m.tabLabel(i))
		l.zones = append(l.zones, zone{r: theme.Rect{X: x, Y: 1, W: lw, H: 1}, kind: zTab, i: i})
		x += lw + 1
	}

	split := !(m.tab == tabHub && m.hub == nil)
	switch {
	case !split:
		l.list = theme.Rect{X: 0, Y: bodyY, W: w, H: bodyH}
	case w >= 90:
		lw := max(36, w*42/100)
		l.list = theme.Rect{X: 0, Y: bodyY, W: lw, H: bodyH}
		l.detail = theme.Rect{X: lw, Y: bodyY, W: w - lw, H: bodyH}
	case bodyH < 8: // too short to stack: show the focused pane only
		l.stacked = true
		if m.focusDetail {
			l.detail = theme.Rect{X: 0, Y: bodyY, W: w, H: bodyH}
		} else {
			l.list = theme.Rect{X: 0, Y: bodyY, W: w, H: bodyH}
		}
	default:
		l.stacked = true
		top := max(4, bodyH/2)
		l.list = theme.Rect{X: 0, Y: bodyY, W: w, H: top}
		l.detail = theme.Rect{X: 0, Y: bodyY + top, W: w, H: bodyH - top}
	}

	if l.list.W > 0 {
		l.zones = append(l.zones, zone{r: l.list, kind: zList})
	}
	if l.detail.W > 0 {
		in := l.detail.Inner()
		l.dLines, l.dPin = m.detailContent(in.W)
		if l.dPin+3 > in.H { // too short to pin a header: scroll everything
			l.dPin = 0
		}
		l.dMax = max(0, len(l.dLines)-in.H)
		off := m.detailScroll
		if m.detailFollow {
			off = l.dMax
		}
		l.dOff = max(0, min(off, l.dMax))
		l.zones = append(l.zones, zone{r: l.detail, kind: zDetail})
	}
	if m.tab != tabHub && l.list.W > 0 {
		l.ids = m.ids()
		in := l.list.Inner()
		l.listRows = max(0, in.H-1) // first inner row holds the column header
		idx := m.index()
		l.listOff = max(0, min(idx-l.listRows/2, len(l.ids)-l.listRows))
		for i := l.listOff; i < min(len(l.ids), l.listOff+l.listRows); i++ {
			l.zones = append(l.zones, zone{r: theme.Rect{X: l.list.X + 1, Y: in.Y + 1 + i - l.listOff, W: l.list.W - 2, H: 1}, kind: zRow, id: l.ids[i]})
		}
	}

	// Footer: "? help  q quit" is right-aligned and always kept; action hints
	// before it are dropped whole (never cut) when they do not fit.
	fy := h - 1
	l.tail = []footItem{{key: "?", label: "help", kind: zHelp}, {key: "q", label: "quit"}}
	tw := itemsWidth(l.tail)
	l.tailX = max(0, w-tw-1)
	room := l.tailX - 3
	x = 1
	for _, it := range m.footerItems() {
		iw := ansi.StringWidth(it.key + " " + it.label)
		if x+iw > room {
			break
		}
		l.head = append(l.head, it)
		if it.kind != zNone {
			l.zones = append(l.zones, zone{r: theme.Rect{X: x, Y: fy, W: iw, H: 1}, kind: it.kind, id: it.id})
		}
		x += iw + 2
	}
	l.zones = append(l.zones, zone{r: theme.Rect{X: l.tailX, Y: fy, W: ansi.StringWidth("? help"), H: 1}, kind: zHelp})

	switch m.overlay {
	case "help":
		l.modalLines = m.helpLines()
		mw := min(w-2, 78)
		mh := min(h-2, len(l.modalLines)+2)
		l.modal = theme.Rect{X: (w - mw) / 2, Y: max(0, (h-mh)/2), W: mw, H: mh}
		l.helpOff = max(0, min(m.helpScroll, len(l.modalLines)-(mh-2)))
		l.zones = []zone{{r: l.modal, kind: zModal}}
	case "confirm":
		mw := min(w-2, 72)
		body := m.confirmBody(max(1, mw-4))
		mh := min(h-2, len(body)+5) // border, blank, body, blank, buttons, border
		l.modal = theme.Rect{X: (w - mw) / 2, Y: max(0, (h-mh)/2), W: mw, H: mh}
		l.zones = []zone{{r: l.modal, kind: zModal}}
		l.buttons = []string{m.confirmLabel(), "Cancel"}
		bx, by := l.modal.X+2, l.modal.Y+l.modal.H-2
		for i, b := range l.buttons {
			bw := theme.ButtonWidth(b)
			if bx+bw <= l.modal.X+l.modal.W-2 {
				l.zones = append(l.zones, zone{r: theme.Rect{X: bx, Y: by, W: bw, H: 1}, kind: zButton, i: i})
			}
			bx += bw + 2
		}
	}
	return l
}

func itemsWidth(items []footItem) int {
	n := 0
	for i, it := range items {
		if i > 0 {
			n += 2
		}
		n += ansi.StringWidth(it.key + " " + it.label)
	}
	return n
}

// footerItems are the contextual hints before "? help  q quit": enabled
// actions first, then navigation.
func (m *Model) footerItems() []footItem {
	var out []footItem
	for _, a := range m.actions() {
		if a.enabled {
			out = append(out, footItem{key: a.keys[0], label: a.label, kind: zAction, id: a.id})
		}
	}
	nav := "select"
	if m.tab == tabHub || m.focusDetail {
		nav = "scroll"
	}
	out = append(out, footItem{key: "↑↓/jk", label: nav}, footItem{key: "tab", label: "focus"}, footItem{key: "y", label: "copy"}, footItem{key: "r", label: "refresh"})
	return out
}

// ---- rendering ----

// View renders the dashboard.
func (m *Model) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}
	l := m.layout()
	lines := []string{m.header(), m.tabsLine()}
	lines = append(lines, m.body(l)...)
	lines = append(lines, m.statusLine(), m.footer(l))
	switch m.overlay {
	case "help":
		lines = m.th.Overlay(lines, m.helpBox(l), l.modal.X, l.modal.Y)
	case "confirm":
		lines = m.th.Overlay(lines, m.confirmBox(l), l.modal.X, l.modal.Y)
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], m.width, "…")
	}
	v := tea.NewView(strings.Join(lines, "\n"))
	v.AltScreen = true
	if m.mouse {
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}

func (m *Model) header() string {
	t := m.th
	badge := t.Badge.Render(" lazylongarm ")
	left := badge + " " + t.Warn.Render("no hub yet — press 3, then S to set one up")
	if m.hub != nil {
		left = badge + " " + t.Muted.Render("hub ") + t.Bold.Render(m.hub.ID) + t.Muted.Render(" · "+m.hub.Address)
	}
	var right []string
	if m.whoami != nil {
		right = append(right, t.Muted.Render(fmt.Sprintf("%s (%s)", m.whoami.Name, m.whoami.Role)))
	}
	if !m.mouse {
		right = append(right, t.Muted.Render("mouse off"))
	}
	switch {
	case m.refreshErr != "":
		right = append(right, t.Err.Render("✗ stale: "+m.refreshErr))
	case m.refreshing && m.lastOK.IsZero():
		right = append(right, t.Pending.Render("loading…"))
	case !m.lastOK.IsZero():
		right = append(right, t.Muted.Render("updated "+ago(m.lastOK)))
	}
	r := strings.Join(right, t.Muted.Render(" · "))
	gap := m.width - ansi.StringWidth(left) - ansi.StringWidth(r) - 1
	if r == "" {
		return left
	}
	if gap < 2 {
		return left + t.Muted.Render(" · ") + r
	}
	return left + strings.Repeat(" ", gap) + r
}

func (m *Model) tabLabel(i int) string {
	n := tabNames[i]
	switch i {
	case tabFleet:
		return fmt.Sprintf(" %d %s (%d) ", i+1, n, len(m.rows))
	case tabJobs:
		active := 0
		for _, j := range m.jobs {
			if j.Job.Active() {
				active++
			}
		}
		return fmt.Sprintf(" %d %s (%d active) ", i+1, n, active)
	}
	return fmt.Sprintf(" %d %s ", i+1, n)
}

func (m *Model) tabsLine() string {
	parts := []string{""}
	for i := range tabNames {
		label := m.tabLabel(i)
		if i == m.tab {
			if !m.th.Color { // keep the active tab visible without color, same width
				label = "[" + label[1:len(label)-1] + "]"
			}
			parts = append(parts, m.th.Badge.Render(label))
		} else {
			parts = append(parts, m.th.Muted.Render(label))
		}
	}
	return strings.Join(parts, " ")[1:] // the leading "" yields a one-cell margin
}

func (m *Model) statusLine() string {
	msg, kind := m.status, m.statusKind
	if msg == "" && m.toolBusy {
		msg, kind = "terminal handed to a command…", "pending"
	}
	if msg == "" {
		return ""
	}
	icon := map[string]string{"ok": "✓", "err": "✗", "pending": "…", "warn": "!"}[kind]
	if icon == "" {
		icon = "·"
	}
	return " " + m.th.Status(kind).Render(icon+" "+msg)
}

func (m *Model) footer(l layout) string {
	t := m.th
	var parts []string
	for _, it := range l.head {
		parts = append(parts, t.KeyHint(it.key, it.label))
	}
	head := " " + strings.Join(parts, "  ")
	var tail []string
	for _, it := range l.tail {
		tail = append(tail, t.KeyHint(it.key, it.label))
	}
	pad := max(2, l.tailX-ansi.StringWidth(head))
	return head + strings.Repeat(" ", pad) + strings.Join(tail, "  ")
}

func (m *Model) body(l layout) []string {
	bodyH := max(1, m.height-4)
	var listBlock, detailBlock []string
	if l.list.W > 0 {
		title, lines := m.listContent(l)
		footer := ""
		if n := len(l.ids); l.listRows > 0 && n > l.listRows {
			footer = fmt.Sprintf("%d/%d", m.index()+1, n)
		}
		listBlock = m.th.Box(lines, l.list.W, l.list.H, theme.BoxOpts{Title: title, Footer: footer, Focused: !m.focusDetail})
	}
	if l.detail.W > 0 {
		in := l.detail.Inner()
		var visible []string
		for _, n := range l.detailRows() {
			line := l.dLines[n]
			if m.drag != nil && m.drag.covers(n) {
				line = m.th.Marked.Render(theme.Fit(ansi.Strip(line), in.W))
			}
			visible = append(visible, line)
		}
		footer := ""
		switch {
		case m.tab == tabJobs && m.detailFollow && l.dOff > 0:
			footer = fmt.Sprintf("↑%d · following", l.dOff)
		case l.dOff < l.dMax && l.dOff > 0:
			footer = fmt.Sprintf("↑%d ↓%d", l.dOff, l.dMax-l.dOff)
		case l.dOff < l.dMax:
			footer = "↓ more"
		case l.dOff > 0:
			footer = fmt.Sprintf("↑%d", l.dOff)
		}
		detailBlock = m.th.Box(visible, l.detail.W, l.detail.H, theme.BoxOpts{Title: m.detailTitle(), Footer: footer, Focused: m.focusDetail})
	}
	var out []string
	switch {
	case listBlock != nil && detailBlock != nil && !l.stacked:
		out = theme.Join(listBlock, detailBlock)
	default:
		out = append(listBlock, detailBlock...)
	}
	for len(out) < bodyH {
		out = append(out, "")
	}
	return out[:bodyH]
}

func (m *Model) detailTitle() string {
	switch m.tab {
	case tabJobs:
		return "Job · log tail"
	case tabHub:
		return "Checks"
	}
	return "Details"
}

// listContent returns the left pane's title and lines (column header first).
func (m *Model) listContent(l layout) (string, []string) {
	t := m.th
	in := l.list.Inner()
	key := func(k string) string { return t.Key.Render(k) }
	switch m.tab {
	case tabHub:
		if m.hub == nil {
			return "Hub", []string{
				"",
				t.Bold.Render("No hub configured."),
				"",
				t.Muted.Render("Press ") + key("S") + t.Muted.Render(" to run the hub setup wizard: pick an SSH host, review the plan,"),
				t.Muted.Render("and lazylongarm installs longarm-hub, opens its ports and enrolls this machine."),
			}
		}
		return "Hub " + m.hub.ID, m.hubInfoLines()
	case tabFleet:
		if len(m.rows) == 0 {
			switch {
			case m.hub == nil:
				return "Workers", []string{"", t.Bold.Render("No hub yet."), t.Muted.Render("Press ") + key("3") + t.Muted.Render(" (Hub), then ") + key("S") + t.Muted.Render(" to run hub setup.")}
			case m.refreshing && m.lastOK.IsZero():
				return "Workers", []string{"", t.Pending.Render("loading workers…")}
			}
			return "Workers", []string{"", t.Bold.Render("No workers yet."), t.Muted.Render("Press ") + key("a") + t.Muted.Render(" to add one (an SSH host or this machine).")}
		}
		cols := fleetColumns(in.W)
		lines := []string{t.Muted.Render(cols.header())}
		sel := m.selected[tabFleet]
		for i := l.listOff; i < min(len(m.rows), l.listOff+l.listRows); i++ {
			r := m.rows[i]
			lines = append(lines, m.fleetRow(r, cols, in.W, r.Name == sel))
		}
		return "Workers", lines
	case tabJobs:
		jobs := m.visibleJobs()
		if len(jobs) == 0 {
			msg := "No active jobs."
			if !m.showDone && len(m.jobs) > 0 {
				return "Jobs", []string{"", t.Bold.Render(msg), t.Muted.Render(fmt.Sprintf("%d older; ", len(m.jobs))) + key("t") + t.Muted.Render(" shows them."), t.Muted.Render("Try: longarm run <worker> -- sleep 120")}
			}
			return "Jobs", []string{"", t.Bold.Render(msg), t.Muted.Render("Try: longarm run <worker> -- sleep 120")}
		}
		ww := 12
		if in.W < 44 {
			ww = 8
		}
		lines := []string{t.Muted.Render(fmt.Sprintf("  %-9s %-*s %-7s %s", "STATE", ww, "WORKER", "TIME", "COMMAND"))}
		sel := m.selected[tabJobs]
		now := time.Now()
		for i := l.listOff; i < min(len(jobs), l.listOff+l.listRows); i++ {
			j := jobs[i]
			name := j.Job.Spec.Name
			if name == "" {
				name = strings.Join(j.Job.Spec.Argv, " ")
			}
			p := m.painter(j.Job.Spec.ID+"@"+j.Job.CreatedAt == sel)
			st := j.Job.ShortState()
			row := p.gutter() + p.r(t.Text, " ") + p.r(t.Status(st), pad(clip(st, 9), 9)) + p.r(t.Text, " ") +
				p.r(t.Text, pad(clip(j.Worker, ww), ww)) + p.r(t.Text, " ") + p.r(t.Muted, pad(clip(j.Job.Duration(now).Round(time.Second).String(), 7), 7)) + p.r(t.Text, " ") + p.r(t.Text, name)
			lines = append(lines, p.fill(row, in.W))
		}
		return "Jobs", lines
	}
	return "", nil
}

type columns struct {
	name     int
	resource bool
}

// fleetColumns sizes the Workers list; the path (relay/direct) lives in the
// details pane. CPU/MEM go first when the pane is narrow.
func fleetColumns(w int) columns {
	c := columns{resource: true}
	fixed := func() int {
		n := 4 + 11 // gutter, space, dot, space; space + state
		if c.resource {
			n += 12
		}
		return n
	}
	if w-fixed() < 10 {
		c.resource = false
	}
	c.name = max(4, min(w-fixed(), 28))
	return c
}

func (c columns) header() string {
	s := "    " + pad("NAME", c.name) + " " + pad("STATE", 10)
	if c.resource {
		s += "    CPU  MEM"
	}
	return s
}

func (m *Model) fleetRow(r fleet.Row, c columns, w int, selected bool) string {
	t := m.th
	p := m.painter(selected)
	state := r.State
	stStyle := t.Text
	switch {
	case r.Online:
		stStyle = t.OK
	case r.State == "pending":
		stStyle = t.Warn
	case !r.InHub:
		stStyle = t.Muted
	default:
		stStyle = t.Err
	}
	name := pad(clip(r.Name, c.name), c.name)
	nameStyle := t.Text
	if selected && m.focusDetail {
		nameStyle = t.Bold
	}
	row := p.gutter() + p.r(t.Text, " ") + p.r(stStyle, dotGlyph(r)) + p.r(t.Text, " ") + p.r(nameStyle, name) + p.r(t.Text, " ") + p.r(stStyle, pad(clip(state, 10), 10))
	if c.resource {
		if r.Online {
			row += p.r(t.Text, " ") + p.r(meterStyle(t, r.CPU), fmt.Sprintf("%5.0f%%", r.CPU)) + p.r(meterStyle(t, r.Mem), fmt.Sprintf("%4.0f%%", r.Mem))
		} else {
			row += p.r(t.Muted, "      -    -")
		}
	}
	return p.fill(row, w)
}

func meterStyle(t theme.Theme, pct float64) lipgloss.Style {
	switch {
	case pct >= 90:
		return t.Err
	case pct >= 70:
		return t.Warn
	}
	return t.Text
}

func dotGlyph(r fleet.Row) string {
	switch {
	case r.Online:
		return "●"
	case r.State == "pending":
		return "◐"
	case !r.InHub:
		return "○"
	}
	return "●"
}

// painter renders the segments of one list row. The selected row of a
// focused list gets the selection background on every segment, so status
// colors survive inside it (a reverse-video of stripped text would lose them).
type painter struct {
	th       theme.Theme
	selected bool
	bg       bool
}

func (m *Model) painter(selected bool) painter {
	return painter{th: m.th, selected: selected, bg: selected && !m.focusDetail}
}

func (p painter) r(st lipgloss.Style, s string) string {
	if p.bg {
		st = st.Inherit(p.th.Sel)
	}
	return st.Render(s)
}

func (p painter) gutter() string {
	if p.selected {
		if p.bg {
			return p.r(p.th.Accent, "▌")
		}
		return p.th.Accent.Render("▌")
	}
	return " "
}

func (p painter) fill(row string, w int) string {
	row = ansi.Truncate(row, w, "…")
	if gap := w - ansi.StringWidth(row); gap > 0 {
		row += p.r(lipgloss.NewStyle(), strings.Repeat(" ", gap))
	}
	return row
}

// detailContent is the right pane's full content and how many leading lines
// stay pinned while the rest scrolls (the job header above its log).
func (m *Model) detailContent(w int) ([]string, int) {
	t := m.th
	switch m.tab {
	case tabFleet:
		r, ok := m.selectedRow()
		if !ok {
			return nil, 0
		}
		dot := t.Status(map[bool]string{true: "ok", false: "err"}[r.Online]).Render(dotGlyph(r) + " " + r.State)
		if !r.InHub {
			dot = t.Muted.Render(dotGlyph(r) + " " + r.State)
		} else if r.State == "pending" {
			dot = t.Warn.Render(dotGlyph(r) + " " + r.State)
		}
		meterW := max(6, min(20, w-22))
		lines := []string{
			t.Bold.Render(r.Name) + "  " + dot,
			"",
			m.kv("platform", r.Platform),
			m.kv("version", r.Version),
			m.kv("path", r.Path) + t.Muted.Render("  relay = through the hub · direct = tcp endpoint"),
			m.kv("owner", r.Owner),
			m.kv("labels", r.LabelText()),
			m.kv("jobs", fmt.Sprint(r.Jobs)),
			m.kv("last seen", m.when(r.LastSeen)),
		}
		if r.Online {
			lines = append(lines, "",
				m.kv("cpu", t.Meter(r.CPU, meterW)+fmt.Sprintf(" %3.0f%%", r.CPU)),
				m.kv("memory", t.Meter(r.Mem, meterW)+fmt.Sprintf(" %3.0f%%", r.Mem)))
		}
		if r.SSHHost != "" || r.Managed {
			managed := ""
			if r.Managed {
				managed = t.Muted.Render("  installed by lazylongarm")
			}
			lines = append(lines, m.kv("ssh host", r.SSHHost)+managed)
		}
		if r.Client != "" {
			lines = append(lines, m.kv("client", r.Client)+t.Muted.Render("  peer: its longarm can dispatch to your workers"))
		}
		if !r.InHub {
			lines = append(lines, "", t.Warn.Render("! Configured here but not in the hub registry."), t.Muted.Render("  Rerun: ")+"lazylongarm worker add "+r.SSHHost)
		}
		if out, ok := m.execOut[r.Name]; ok {
			lines = append(lines, "", t.Accent.Render("longarm exec "+r.Name+" -- uname -a"))
			for _, l := range strings.Split(out, "\n") {
				lines = append(lines, "  "+connection.Sanitize(l))
			}
		}
		return lines, 0
	case tabJobs:
		j, ok := m.selectedJob()
		if !ok {
			return nil, 0
		}
		jb := j.Job
		exit := "-"
		if jb.HasExitCode {
			exit = fmt.Sprint(jb.ExitCode)
		}
		title := jb.Spec.Name
		if title == "" {
			title = clip(strings.Join(jb.Spec.Argv, " "), max(10, w/2))
		}
		worker := j.Worker
		if !j.WorkerOnline {
			worker += t.Err.Render(" (offline)")
		}
		lines := []string{
			t.Bold.Render(dash(title)) + "  " + t.Status(jb.ShortState()).Render(jb.ShortState()),
			"",
			m.kv("id", jb.Spec.ID),
			m.kv("worker", worker),
			m.kv("command", strings.Join(jb.Spec.Argv, " ")),
			m.kv("owner", jb.Spec.Owner),
			m.kv("started", m.when(jb.StartedAt)),
			m.kv("duration", jb.Duration(time.Now()).String()),
			m.kv("exit", strings.TrimSpace(exit+" "+jb.Reason)),
			"",
			t.Muted.Render("── log " + strings.Repeat("─", max(0, w-7))),
		}
		pin := len(lines)
		st, have := m.logs[jb.Spec.ID]
		switch {
		case !have || (st.pending && st.text == ""):
			lines = append(lines, t.Pending.Render("loading log…"))
		case st.err != "":
			lines = append(lines, t.Err.Render("✗ log: "+st.err))
		}
		if text := strings.TrimRight(connection.Sanitize(st.text), "\n"); text != "" {
			lines = append(lines, strings.Split(text, "\n")...)
		}
		return lines, pin
	case tabHub:
		return m.hubCheckLines(), 0
	}
	return nil, 0
}

func (m *Model) hubInfoLines() []string {
	h := m.hub
	panel, hint := m.panelLine(h)
	lines := []string{
		m.kv("address", h.Address),
		m.kv("mode", h.Mode),
		m.kv("ssh host", h.SSHHost),
		m.kv("panel", panel),
		pad("", 12) + hint,
		m.kv("fingerprint", h.CAFingerprint),
		m.kv("version", h.Version),
	}
	if h.Cloud != nil {
		lines = append(lines, m.kv("firewall", fmt.Sprintf("%s %s/%s rules %s", h.Cloud.Provider, h.Cloud.ResourceGroup, h.Cloud.VM, strings.Join(h.Cloud.Rules, ","))))
	}
	return lines
}

func (m *Model) hubCheckLines() []string {
	t := m.th
	if m.hub == nil {
		return nil
	}
	lines := []string{t.Accent.Render("hub checks")}
	if m.hubPending && m.hubChecks == nil {
		lines = append(lines, t.Pending.Render("  checking (ssh, healthz, firewall, enrollment)…"))
	}
	for _, c := range m.hubChecks {
		lines = append(lines, fmt.Sprintf("  %s %s %s", t.Mark(c.Status), t.Bold.Render(pad(c.ID, 10)), c.Detail))
		if c.Fix != "" {
			lines = append(lines, t.Muted.Render("      fix: "+c.Fix))
		}
	}
	if len(m.doctor) > 0 {
		lines = append(lines, "", t.Accent.Render("longarm doctor"))
		for _, d := range m.doctor {
			lines = append(lines, fmt.Sprintf("  %s %s %s", t.Mark(d.Status), t.Bold.Render(pad(clip(d.Title, 16), 16)), d.Detail))
			if d.Fix != "" && d.Status != "ok" {
				lines = append(lines, t.Muted.Render("      fix: "+d.Fix))
			}
		}
	}
	return lines
}

// panelLine is the panel value and a hint line for the keys around it.
func (m *Model) panelLine(h *config.Hub) (string, string) {
	t := m.th
	if u := m.PanelURL(); u != "" {
		return t.OK.Render("● " + u), t.Muted.Render("tunnel open · p closes · y copies the URL")
	}
	if u := h.PanelURL(); u != "" {
		return t.OK.Render(u), t.Muted.Render(h.PanelMode() + " · p opens it · A changes access")
	}
	return dash(h.Panel), t.Muted.Render("p opens a tunnel + browser · U adds a login")
}

func (m *Model) helpLines() []string {
	t := m.th
	k := func(keys, desc string) string { return "  " + t.Key.Render(pad(keys, 12)) + " " + desc }
	lines := []string{t.Accent.Render(tabNames[m.tab] + " actions")}
	for _, a := range m.actions() {
		line := k(strings.Join(a.keys, "/"), a.label)
		if !a.enabled {
			line = "  " + t.Muted.Render(pad(strings.Join(a.keys, "/"), 12)+" "+a.label+"  (not available here)")
		}
		lines = append(lines, line)
	}
	lines = append(lines, "", t.Accent.Render("Navigation"),
		k("1 2 3", "switch tab (also ←/→, h/l)"),
		k("↑↓ j k", "select; gg / G first / last"),
		k("tab", "focus the right pane (j/k, PgUp/PgDn scroll); shift+tab back"),
		k("y", "copy the log / details / panel URL to the clipboard"),
		k("r", "refresh now (auto every "+m.opts.Refresh.String()+")"),
		k("M", "mouse on/off (off = the terminal's own text selection)"),
		k("q", "quit; Ctrl+C always quits"),
		"", t.Accent.Render("Mouse"),
		"  click a tab, a row or a footer hint; the wheel scrolls the pane under it",
		"  drag over the right pane to copy whole lines (log, details)",
		"  "+t.Muted.Render("while the mouse is on, Shift- or Option-drag selects text natively"),
		"", t.Accent.Render("Symbols"),
		"  "+t.OK.Render("●")+" online  "+t.Err.Render("●")+" offline  "+t.Warn.Render("◐")+" pending approval  "+t.Muted.Render("○")+" configured here, not in hub",
		"  "+t.Muted.Render("Wizards (a, P, S) and longarm commands (s, f) take over the"),
		"  "+t.Muted.Render("terminal and return here."),
	)
	return lines
}

func (m *Model) helpBox(l layout) []string {
	inner := l.modal.H - 2
	body := l.modalLines[l.helpOff:]
	footer := "Esc closes"
	if len(l.modalLines) > inner {
		footer = fmt.Sprintf("%d/%d · j/k scroll · Esc closes", min(len(l.modalLines), l.helpOff+inner), len(l.modalLines))
	}
	return m.th.Box(body, l.modal.W, l.modal.H, theme.BoxOpts{Title: "Help · " + tabNames[m.tab], Footer: footer, Focused: true})
}

func (m *Model) confirmLabel() string {
	if m.confirm != nil && m.confirm.button != "" {
		return m.confirm.button
	}
	return "OK"
}

func (m *Model) confirmBody(w int) []string {
	if m.confirm == nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(m.confirm.body, "\n") {
		if l == "" {
			out = append(out, "")
			continue
		}
		out = append(out, strings.Split(ansi.Wordwrap(l, w, " "), "\n")...)
	}
	return out
}

func (m *Model) confirmBox(l layout) []string {
	body := m.confirmBody(max(1, l.modal.W-4))
	lines := append([]string{""}, body...)
	for len(lines) < l.modal.H-3 {
		lines = append(lines, "")
	}
	lines = lines[:max(0, l.modal.H-3)]
	var buttons []string
	for i, b := range l.buttons {
		buttons = append(buttons, m.th.ButtonText(b, i == m.confirmFocus))
	}
	lines = append(lines, strings.Join(buttons, "  "))
	title := ""
	if m.confirm != nil {
		title = m.confirm.title
	}
	return m.th.Box(lines, l.modal.W, l.modal.H, theme.BoxOpts{Title: title, Footer: "y " + strings.ToLower(m.confirmLabel()) + " · n/Esc cancel", Danger: true})
}

// when shows an RFC 3339 time as "3m ago · 14:05:09" (raw text otherwise).
func (m *Model) when(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || s == "" {
		return s
	}
	return ago(t) + m.th.Muted.Render(" · "+t.Local().Format("Jan 2 15:04:05"))
}

func (m *Model) kv(k, v string) string {
	return m.th.Muted.Render(pad(k, 11)) + " " + dash(v)
}

func pad(s string, n int) string {
	if w := ansi.StringWidth(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

func dash(s string) string {
	if strings.TrimSpace(ansi.Strip(s)) == "" {
		return "-"
	}
	return s
}

func clip(s string, n int) string {
	if ansi.StringWidth(s) <= n {
		return s
	}
	return ansi.Truncate(s, n, "…")
}

func ago(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	if d < 2*time.Second {
		return "just now"
	}
	return d.String() + " ago"
}
