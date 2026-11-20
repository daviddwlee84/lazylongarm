package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/fleet"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
)

func (m *Model) st(color string, s string) string {
	if !m.opts.Color {
		return s
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render(s)
}
func (m *Model) bold(s string) string {
	if !m.opts.Color {
		return s
	}
	return lipgloss.NewStyle().Bold(true).Render(s)
}
func (m *Model) rev(s string) string {
	if !m.opts.Color {
		return "> " + strings.TrimPrefix(s, "  ")
	}
	return lipgloss.NewStyle().Reverse(true).Render(s)
}

// View renders the dashboard.
func (m *Model) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}
	w, h := m.width, m.height
	header := m.header()
	tabs := m.tabsLine()
	status := m.statusLine()
	footer := m.footer()
	bodyH := max(1, h-4)
	var body []string
	switch m.overlay {
	case "help":
		body = m.helpLines(w)
	case "confirm":
		body = m.confirmLines(w)
	default:
		body = m.body(w, bodyH)
	}
	lines := []string{header, tabs}
	lines = append(lines, fit(body, bodyH)...)
	lines = append(lines, status, footer)
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], w, "…")
	}
	v := tea.NewView(strings.Join(lines, "\n"))
	v.AltScreen = true
	return v
}

func fit(lines []string, n int) []string {
	if len(lines) > n {
		return lines[:n]
	}
	for len(lines) < n {
		lines = append(lines, "")
	}
	return lines
}

func (m *Model) header() string {
	hub := m.st("11", "no hub — press 3 then S to set one up")
	if m.hub != nil {
		hub = fmt.Sprintf("hub %s %s", m.bold(m.hub.ID), m.st("8", m.hub.Address))
	}
	who := ""
	if m.whoami != nil {
		who = m.st("8", fmt.Sprintf(" · %s (%s)", m.whoami.Name, m.whoami.Role))
	}
	fresh := ""
	switch {
	case m.refreshErr != "":
		fresh = m.st("9", " · stale: "+m.refreshErr)
	case !m.lastOK.IsZero():
		fresh = m.st("8", " · updated "+ago(m.lastOK))
	case m.refreshing:
		fresh = m.st("8", " · loading…")
	}
	return m.st("10", "lazylongarm") + " · " + hub + who + fresh
}

func (m *Model) tabsLine() string {
	var parts []string
	for i, n := range tabNames {
		label := fmt.Sprintf(" %d %s ", i+1, n)
		switch i {
		case tabFleet:
			label = fmt.Sprintf(" %d %s (%d) ", i+1, n, len(m.rows))
		case tabJobs:
			active := 0
			for _, j := range m.jobs {
				if j.Job.Active() {
					active++
				}
			}
			label = fmt.Sprintf(" %d %s (%d active) ", i+1, n, active)
		}
		if i == m.tab {
			parts = append(parts, m.rev(label))
		} else {
			parts = append(parts, m.st("8", label))
		}
	}
	return strings.Join(parts, " ")
}

func (m *Model) statusLine() string {
	if m.status != "" {
		return m.st("14", m.status)
	}
	if m.toolBusy {
		return m.st("14", "terminal handed to a command…")
	}
	return ""
}

// footer keeps "? help  q quit" visible at every width; the action hints in
// front of it are truncated first.
func (m *Model) footer() string {
	var parts []string
	for _, a := range m.actions() {
		if a.enabled {
			parts = append(parts, m.bold(a.keys[0])+" "+a.label)
		}
	}
	parts = append(parts, m.bold("↑↓/jk")+" select", m.bold("tab")+" focus", m.bold("r")+" refresh")
	tail := m.bold("?") + " help  " + m.bold("q") + " quit"
	room := m.width - ansi.StringWidth(tail) - 2
	head := ansi.Truncate(strings.Join(parts, "  "), max(0, room), "…")
	return head + "  " + tail
}

func (m *Model) body(w, h int) []string {
	switch m.tab {
	case tabHub:
		return m.hubLines(w)
	}
	list, detail := m.listLines(), m.detailLines(w)
	if m.focusDetail {
		detail = scroll(detail, m.detailScroll)
	}
	if w < 90 {
		top := max(3, h/2)
		out := fit(list, top)
		out = append(out, m.st("8", strings.Repeat("─", w)))
		return append(out, detail...)
	}
	lw := max(32, w*42/100)
	listT := m.paneTitle("Workers", !m.focusDetail)
	detT := m.paneTitle("Details", m.focusDetail)
	if m.tab == tabJobs {
		listT = m.paneTitle("Jobs", !m.focusDetail)
		detT = m.paneTitle("Job · log tail", m.focusDetail)
	}
	list = append([]string{listT}, list...)
	detail = append([]string{detT}, detail...)
	out := make([]string, h)
	for i := 0; i < h; i++ {
		l, d := "", ""
		if i < len(list) {
			l = list[i]
		}
		if i < len(detail) {
			d = detail[i]
		}
		l = ansi.Truncate(l, lw, "…")
		pad := lw - ansi.StringWidth(l)
		out[i] = l + strings.Repeat(" ", max(0, pad)) + m.st("8", " │ ") + ansi.Truncate(d, max(1, w-lw-3), "…")
	}
	return out
}

func (m *Model) paneTitle(t string, focused bool) string {
	if focused {
		return m.bold("▌" + t)
	}
	return m.st("8", " "+t)
}

func scroll(lines []string, n int) []string {
	if n >= len(lines) {
		n = max(0, len(lines)-1)
	}
	return lines[n:]
}

func (m *Model) listLines() []string {
	var out []string
	switch m.tab {
	case tabFleet:
		if len(m.rows) == 0 {
			if m.hub == nil {
				return []string{"", "  No hub yet.", "  Press 3 (Hub) then S to run hub setup."}
			}
			if m.refreshing && m.lastOK.IsZero() {
				return []string{"", "  loading workers…"}
			}
			return []string{"", "  No workers yet.", "  Press a to add one (an SSH host or this machine)."}
		}
		sel := m.selected[tabFleet]
		for _, r := range m.rows {
			line := fmt.Sprintf("  %s %-18s %-10s %-7s %3.0f%% %3.0f%%", m.dot(r), clip(r.Name, 18), r.State, dash(r.Path), r.CPU, r.Mem)
			if r.Name == sel {
				line = m.rev(ansi.Strip(line))
			}
			out = append(out, line)
		}
	case tabJobs:
		jobs := m.visibleJobs()
		if len(jobs) == 0 {
			msg := "  No active jobs."
			if !m.showDone && len(m.jobs) > 0 {
				msg = fmt.Sprintf("  No active jobs (%d older; t shows them).", len(m.jobs))
			}
			return []string{"", msg, "  Try: longarm run <worker> -- sleep 120"}
		}
		sel := m.selected[tabJobs]
		now := time.Now()
		for _, j := range jobs {
			name := j.Job.Spec.Name
			if name == "" {
				name = strings.Join(j.Job.Spec.Argv, " ")
			}
			line := fmt.Sprintf("  %-8s %-14s %-8s %s", j.Job.ShortState(), clip(j.Worker, 14), j.Job.Duration(now), name)
			if j.Job.Spec.ID+"@"+j.Job.CreatedAt == sel {
				line = m.rev(line)
			} else {
				line = m.jobColor(j.Job, line)
			}
			out = append(out, line)
		}
	}
	return out
}

func (m *Model) dot(r fleet.Row) string {
	switch {
	case r.Online:
		return m.st("10", "●")
	case r.State == "pending":
		return m.st("11", "◐")
	case !r.InHub:
		return m.st("8", "○")
	default:
		return m.st("9", "●")
	}
}

func (m *Model) jobColor(j longarm.Job, s string) string {
	switch j.ShortState() {
	case "running":
		return m.st("10", s)
	case "failed", "lost":
		return m.st("9", s)
	case "killed":
		return m.st("11", s)
	}
	return s
}

func (m *Model) detailLines(w int) []string {
	switch m.tab {
	case tabFleet:
		r, ok := m.selectedRow()
		if !ok {
			return nil
		}
		lines := []string{
			m.bold(r.Name) + "  " + r.State,
			kv("platform", r.Platform),
			kv("version", r.Version),
			kv("path", r.Path+m.st("8", "  (relay = through the hub; direct = tcp endpoint)")),
			kv("owner", r.Owner),
			kv("cpu", bar(r.CPU, 20)+fmt.Sprintf(" %.0f%%", r.CPU)),
			kv("memory", bar(r.Mem, 20)+fmt.Sprintf(" %.0f%%", r.Mem)),
			kv("jobs", fmt.Sprint(r.Jobs)),
			kv("last seen", r.LastSeen),
		}
		if r.SSHHost != "" || r.Managed {
			lines = append(lines, kv("ssh host", dash(r.SSHHost)+m.st("8", map[bool]string{true: "  (installed by lazylongarm)", false: ""}[r.Managed])))
		}
		if !r.InHub {
			lines = append(lines, "", m.st("11", "Configured here but not in the hub registry."), "Rerun: lazylongarm worker add "+r.SSHHost)
		}
		if out, ok := m.execOut[r.Name]; ok {
			lines = append(lines, "", m.st("8", "longarm exec "+r.Name+" -- uname -a"))
			for _, l := range strings.Split(out, "\n") {
				lines = append(lines, "  "+connection.Sanitize(l))
			}
		}
		return lines
	case tabJobs:
		j, ok := m.selectedJob()
		if !ok {
			return nil
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
		lines := []string{
			m.bold(dash(title)) + "  " + m.jobColor(jb, jb.ShortState()),
			kv("id", jb.Spec.ID),
			kv("worker", j.Worker+map[bool]string{true: "", false: m.st("9", " (offline)")}[j.WorkerOnline]),
			kv("command", strings.Join(jb.Spec.Argv, " ")),
			kv("owner", jb.Spec.Owner),
			kv("started", jb.StartedAt),
			kv("duration", jb.Duration(time.Now()).String()),
			kv("exit", exit+" "+jb.Reason),
			"",
		}
		st, have := m.logs[jb.Spec.ID]
		switch {
		case !have || (st.pending && st.text == ""):
			lines = append(lines, m.st("8", "loading log…"))
		case st.err != "":
			lines = append(lines, m.st("9", "log: "+st.err))
		}
		logLines := strings.Split(strings.TrimRight(connection.Sanitize(st.text), "\n"), "\n")
		room := max(3, m.height-6-len(lines))
		if !m.focusDetail && len(logLines) > room {
			logLines = logLines[len(logLines)-room:]
		}
		for _, l := range logLines {
			lines = append(lines, m.st("7", l))
		}
		return lines
	}
	return nil
}

func (m *Model) hubLines(w int) []string {
	if m.hub == nil {
		return []string{"", "  No hub configured.", "", "  Press S to run the hub setup wizard: pick an SSH host, review the plan,", "  and lazylongarm installs longarm-hub, opens its ports and enrolls this machine."}
	}
	h := m.hub
	lines := []string{
		m.bold("hub " + h.ID),
		kv("address", h.Address),
		kv("mode", h.Mode),
		kv("ssh host", dash(h.SSHHost)),
		kv("panel", m.panelLine(h.Panel)),
		kv("fingerprint", h.CAFingerprint),
		kv("version", dash(h.Version)),
	}
	if h.Cloud != nil {
		lines = append(lines, kv("firewall", fmt.Sprintf("%s %s/%s rules %s", h.Cloud.Provider, h.Cloud.ResourceGroup, h.Cloud.VM, strings.Join(h.Cloud.Rules, ","))))
	}
	lines = append(lines, "", m.bold("checks"))
	if m.hubPending && m.hubChecks == nil {
		lines = append(lines, m.st("8", "  checking (ssh, healthz, firewall, enrollment)…"))
	}
	for _, c := range m.hubChecks {
		lines = append(lines, fmt.Sprintf("  %s %-10s %s", m.mark(c.Status), c.ID, c.Detail))
		if c.Fix != "" {
			lines = append(lines, m.st("8", "      fix: "+c.Fix))
		}
	}
	if len(m.doctor) > 0 {
		lines = append(lines, "", m.bold("longarm doctor"))
		for _, d := range m.doctor {
			lines = append(lines, fmt.Sprintf("  %s %-16s %s", m.mark(d.Status), clip(d.Title, 16), d.Detail))
			if d.Fix != "" && d.Status != "ok" {
				lines = append(lines, m.st("8", "      fix: "+d.Fix))
			}
		}
	}
	return lines
}

func (m *Model) panelLine(remote string) string {
	if u := m.PanelURL(); u != "" {
		return m.st("10", u) + m.st("8", "  (tunnel open · p closes)")
	}
	return dash(remote) + m.st("8", "  (p opens a tunnel + browser · U creates a login)")
}

func (m *Model) mark(status string) string {
	switch status {
	case "ok":
		return m.st("10", "✓")
	case "warn":
		return m.st("11", "!")
	case "skip":
		return m.st("8", "-")
	case "info":
		return m.st("14", "i")
	default:
		return m.st("9", "✗")
	}
}

func (m *Model) helpLines(w int) []string {
	lines := []string{m.bold("Help · " + tabNames[m.tab]), ""}
	for _, a := range m.actions() {
		state := ""
		if !a.enabled {
			state = m.st("8", "  (not available here)")
		}
		lines = append(lines, fmt.Sprintf("  %-10s %s%s", strings.Join(a.keys, "/"), a.label, state))
	}
	lines = append(lines, "",
		"  1 2 3      switch tab (also ←/→, h/l)",
		"  ↑↓ j k     select; gg / G first / last",
		"  tab        focus details pane (scroll with j/k), shift+tab back",
		"  r          refresh now (auto every "+m.opts.Refresh.String()+")",
		"  q          quit; Ctrl+C always quits",
		"",
		"  Symbols: ● online  ● offline (red)  ◐ pending approval  ○ configured here, not in hub",
		"  Wizards (a, S) and longarm commands (s, f) take over the terminal and return here.",
		"", m.st("8", "  Esc / ? closes help"))
	return lines
}

func (m *Model) confirmLines(w int) []string {
	if m.confirm == nil {
		return nil
	}
	lines := []string{"", m.bold("  " + m.confirm.title), ""}
	for _, l := range strings.Split(m.confirm.body, "\n") {
		lines = append(lines, "  "+l)
	}
	return append(lines, "", "  "+m.bold("y")+" kill   "+m.bold("n / Esc")+" cancel (default)")
}

func kv(k, v string) string { return fmt.Sprintf("%-11s %s", k, dash(v)) }

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
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

func bar(pct float64, width int) string {
	n := int(pct / 100 * float64(width))
	n = max(0, min(n, width))
	return strings.Repeat("█", n) + strings.Repeat("░", width-n)
}

func ago(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	if d < 2*time.Second {
		return "just now"
	}
	return d.String() + " ago"
}
