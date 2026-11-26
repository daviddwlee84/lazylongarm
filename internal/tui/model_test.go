package tui

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/daviddwlee84/lazylongarm/internal/ui/theme"
)

func key(s string) tea.KeyPressMsg {
	switch s {
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	}
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: s}
}

func newModel() *Model {
	cfg := &config.Config{Hubs: []config.Hub{{ID: "hub-1", Mode: "public", Address: "hub.example.com:7443", SSHHost: "my-vps"}}, DefaultHub: "hub-1"}
	m := New(Options{Config: cfg})
	m.width, m.height = 120, 30
	return m
}

func nodes(names ...string) []longarm.Node {
	var out []longarm.Node
	for _, n := range names {
		out = append(out, longarm.Node{Name: n, Role: "worker", State: "approved", Online: true})
	}
	return out
}

func TestSelectionFollowsIdentityAcrossRefresh(t *testing.T) {
	m := newModel()
	m.refreshGen = 1
	m.Update(refreshMsg{gen: 1, nodes: nodes("a", "b", "c")})
	m.Update(key("j"))
	if m.selected[tabFleet] != "b" {
		t.Fatalf("selected %q", m.selected[tabFleet])
	}
	m.refreshGen = 2
	m.Update(refreshMsg{gen: 2, nodes: nodes("z", "b", "a")})
	if m.selected[tabFleet] != "b" {
		t.Fatalf("selection jumped to %q", m.selected[tabFleet])
	}
	// A late result from an older generation is ignored.
	m.Update(refreshMsg{gen: 1, nodes: nodes("only")})
	if len(m.rows) != 3 {
		t.Fatalf("stale refresh applied: %v", m.rows)
	}
}

func TestFailedRefreshKeepsRowsAndMarksStale(t *testing.T) {
	m := newModel()
	m.refreshGen = 1
	m.Update(refreshMsg{gen: 1, nodes: nodes("a")})
	m.refreshGen = 2
	m.Update(refreshMsg{gen: 2, nodesErr: errString("hub unreachable"), jobsErr: errString("hub unreachable")})
	if len(m.rows) != 1 || !strings.Contains(m.refreshErr, "hub unreachable") {
		t.Fatalf("rows %v err %q", m.rows, m.refreshErr)
	}
	if !strings.Contains(viewText(m), "stale") {
		t.Fatal("stale marker not rendered")
	}
}

func TestTabsHelpAndKillConfirmation(t *testing.T) {
	m := newModel()
	m.refreshGen = 1
	m.Update(refreshMsg{gen: 1, nodes: nodes("a"), jobs: []longarm.FleetJob{{Worker: "a", Job: longarm.Job{State: "JOB_STATE_RUNNING", CreatedAt: "2026-11-20T00:00:00Z", Spec: longarm.JobSpec{ID: "01JOB", Name: "sleep", Argv: []string{"sleep", "600"}}}}}})
	m.Update(key("2"))
	if m.tab != tabJobs {
		t.Fatalf("tab %d", m.tab)
	}
	m.Update(key("K"))
	if m.overlay != "confirm" || !strings.Contains(viewText(m), "Kill") {
		t.Fatalf("overlay %q", m.overlay)
	}
	// q inside the confirmation cancels instead of quitting.
	_, cmd := m.Update(key("q"))
	if m.overlay != "" || cmd != nil || m.quitting {
		t.Fatalf("q in confirm: overlay %q quitting %v", m.overlay, m.quitting)
	}
	m.Update(key("?"))
	if m.overlay != "help" || !strings.Contains(viewText(m), "follow log") {
		t.Fatal("help does not list tab actions")
	}
	m.Update(key("esc"))
	m.Update(key("l"))
	if m.tab != tabHub {
		t.Fatalf("l should move to the next tab, got %d", m.tab)
	}
}

func TestNarrowLayoutStillShowsFooter(t *testing.T) {
	m := newModel()
	m.width, m.height = 60, 16
	m.refreshGen = 1
	m.Update(refreshMsg{gen: 1, nodes: nodes("工作站-👩🏽‍💻", "b")})
	out := viewText(m)
	lines := strings.Split(out, "\n")
	if len(lines) != 16 || !strings.Contains(lines[len(lines)-1], "quit") {
		t.Fatalf("lines %d last %q", len(lines), lines[len(lines)-1])
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func viewText(m *Model) string { return m.View().Content }

func TestFleetCompanionActions(t *testing.T) {
	cfg := &config.Config{Hubs: []config.Hub{{ID: "hub-1", Mode: "public", Address: "hub.example.com:7443"}}, DefaultHub: "hub-1",
		Targets: []config.Target{{ID: "a", Name: "a", SSHHost: "worker-a", Hub: "hub-1", WorkerName: "a"}}}
	m := New(Options{Config: cfg})
	m.width, m.height = 120, 30
	m.refreshGen = 1
	m.Update(refreshMsg{gen: 1, nodes: nodes("a", "b")})
	enabled := func() map[string]bool {
		out := map[string]bool{}
		for _, a := range m.actions() {
			out[a.id] = a.enabled
		}
		return out
	}
	m.selected[tabFleet] = "a"
	if e := enabled(); !e["add"] || !e["peer"] {
		t.Fatalf("add actions with a hub: %v", e)
	}
	if e := enabled(); !e["remotes"] || !e["dotfiles"] {
		t.Fatalf("managed row: %v", e)
	}
	m.selected[tabFleet] = "b" // in the hub only: no SSH host to reach it
	if e := enabled(); e["remotes"] || e["dotfiles"] {
		t.Fatalf("unmanaged row: %v", e)
	}
	m.Update(key("?"))
	if !strings.Contains(viewText(m), "hc/herdr remote") || !strings.Contains(viewText(m), "dotfiles") {
		t.Fatal("help lacks companion actions")
	}
}

func TestPanelKeyOpensDirectURLWithoutTunnel(t *testing.T) {
	cfg := &config.Config{Hubs: []config.Hub{{ID: "hub-1", Mode: "public", Address: "hub.example.com:7443", SSHHost: "my-vps",
		PanelAccess: &config.PanelAccess{Mode: config.PanelTailnet, URL: "https://hub-1.tail1234.ts.net/"}}}, DefaultHub: "hub-1"}
	var opened, checked []string
	onTailnet := true
	m := New(Options{Config: cfg, OpenURL: func(u string) error { opened = append(opened, u); return nil },
		TailnetCheck: func(_ context.Context, u string) (bool, string) {
			checked = append(checked, u)
			if onTailnet {
				return true, ""
			}
			return false, "Tailscale is not installed on this machine"
		}})
	m.width, m.height = 120, 30
	m.tab = tabHub

	// On the tailnet: p checks, then opens the URL; no tunnel.
	cmd := m.togglePanel()
	if cmd == nil {
		t.Fatal("p did not check Tailscale")
	}
	m.Update(cmd())
	if m.panelPending || len(checked) != 1 || len(opened) != 1 || opened[0] != "https://hub-1.tail1234.ts.net/" {
		t.Fatalf("pending %v checked %v opened %v", m.panelPending, checked, opened)
	}
	ids := map[string]bool{}
	for _, a := range m.actions() {
		ids[a.id] = a.enabled
	}
	if !ids["panel-access"] || !ids["panel"] {
		t.Fatalf("hub actions: %v", ids)
	}
	if v, hint := m.panelLine(&cfg.Hubs[0]); !strings.Contains(v, "tail1234.ts.net") || !strings.Contains(hint, "p opens it") {
		t.Fatal(v, hint)
	}

	// Off the tailnet: p falls back to the SSH tunnel and says why.
	onTailnet = false
	_, next := m.Update(m.togglePanel()())
	if next == nil || !m.panelPending || len(opened) != 1 || !strings.Contains(m.panelNote, "not installed") {
		t.Fatalf("no tunnel fallback: pending %v note %q opened %v", m.panelPending, m.panelNote, opened)
	}
}

// A public URL opens directly, without asking Tailscale.
func TestPanelPublicURLSkipsTailnetCheck(t *testing.T) {
	cfg := &config.Config{Hubs: []config.Hub{{ID: "hub-1", Mode: "public", Address: "hub.example.com:7443", SSHHost: "my-vps",
		PanelAccess: &config.PanelAccess{Mode: config.PanelPublic, URL: "https://hub.example.com/", Domain: "hub.example.com"}}}, DefaultHub: "hub-1"}
	var opened []string
	checks := 0
	m := New(Options{Config: cfg, OpenURL: func(u string) error { opened = append(opened, u); return nil },
		TailnetCheck: func(context.Context, string) (bool, string) { checks++; return true, "" }})
	m.width, m.height = 120, 30
	m.Update(key("3"))
	if cmd := m.togglePanel(); cmd != nil || len(opened) != 1 || checks != 0 || m.panelPending {
		t.Fatalf("public: cmd %v opened %v checks %d", cmd != nil, opened, checks)
	}
}

// screenPos finds text on the rendered screen (zero-based cell coordinates).
func screenPos(t *testing.T, m *Model, text string) (int, int) {
	t.Helper()
	for y, l := range strings.Split(ansi.Strip(viewText(m)), "\n") {
		if x := strings.Index(l, text); x >= 0 {
			return ansi.StringWidth(l[:x]), y
		}
	}
	t.Fatalf("%q not on screen:\n%s", text, ansi.Strip(viewText(m)))
	return 0, 0
}

func clickAt(m *Model, x, y int) {
	m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
}

func TestMouseTabsRowsAndWheel(t *testing.T) {
	m := demoModel(true, 120, 32)
	clickOn(t, m, "2 Jobs")
	if m.tab != tabJobs {
		t.Fatalf("tab %d", m.tab)
	}
	clickOn(t, m, "1 Fleet")
	x, y := mustPos(t, m, "gpu-01")
	clickAt(m, x, y)
	if m.selected[tabFleet] != "gpu-01" || m.focusDetail {
		t.Fatalf("row click: %q focusDetail %v", m.selected[tabFleet], m.focusDetail)
	}
	m.Update(tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelDown})
	if m.selected[tabFleet] == "gpu-01" {
		t.Fatal("wheel over the list did not move the selection")
	}
}

func mustPos(t *testing.T, m *Model, text string) (int, int) { return screenPos(t, m, text) }

func clickOn(t *testing.T, m *Model, text string) {
	t.Helper()
	x, y := screenPos(t, m, text)
	clickAt(m, x, y)
}

func TestMouseFooterActionNeedsSameTargetAndState(t *testing.T) {
	m := demoModel(true, 120, 32)
	m.Update(key("2"))
	x, y := mustPos(t, m, "K kill")
	// Released elsewhere: nothing.
	m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: x + 30, Y: y, Button: tea.MouseLeft})
	if m.overlay != "" {
		t.Fatal("release off the hint ran the action")
	}
	// The layout changed between press and release: nothing.
	m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m.Update(tea.WindowSizeMsg{Width: 121, Height: 32})
	m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	if m.overlay != "" {
		t.Fatal("stale press ran the action after a resize")
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	clickAt(m, x, y)
	if m.overlay != "confirm" || m.confirmFocus != 1 {
		t.Fatalf("kill hint: overlay %q focus %d", m.overlay, m.confirmFocus)
	}
	// The modal owns the mouse: a click on a row behind it does nothing,
	// and clicking Cancel closes it without killing.
	clickAt(m, 5, 4)
	if m.overlay != "confirm" {
		t.Fatal("click outside the confirmation closed or passed through it")
	}
	clickOn(t, m, "Cancel")
	if m.overlay != "" || m.status != "canceled" {
		t.Fatalf("cancel button: overlay %q status %q", m.overlay, m.status)
	}
}

func TestMouseToggleAndOption(t *testing.T) {
	m := demoModel(true, 120, 32)
	if m.View().MouseMode != tea.MouseModeCellMotion {
		t.Fatal("mouse should be captured by default")
	}
	m.Update(key("M"))
	if m.View().MouseMode != tea.MouseModeNone {
		t.Fatal("M did not release the mouse")
	}
	clickOn(t, m, "2 Jobs")
	if m.tab != tabFleet {
		t.Fatal("clicks must be ignored while the mouse is off")
	}
	if New(Options{}).View().MouseMode != tea.MouseModeNone {
		t.Fatal("Options.Mouse=false must not capture the mouse")
	}
}

func TestDragCopiesWholeLines(t *testing.T) {
	m := demoModel(false, 120, 32)
	var copied string
	m.opts.Clipboard = func(s string) error { copied = s; return nil }
	m.Update(key("2"))
	x, y := mustPos(t, m, "TestCase38 (0.38s)")
	m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: x + 2, Y: y + 1, Button: tea.MouseLeft})
	_, cmd := m.Update(tea.MouseReleaseMsg{X: x + 2, Y: y + 1, Button: tea.MouseLeft})
	if cmd == nil {
		t.Fatal("no copy command")
	}
	for _, msg := range runBatch(cmd) {
		m.Update(msg)
	}
	if copied != "--- PASS: TestCase38 (0.38s)\n=== RUN   TestCase39" {
		t.Fatalf("copied %q", copied)
	}
	if !strings.Contains(m.status, "copied 2 lines") {
		t.Fatalf("status %q", m.status)
	}
	// A plain click (no drag) only focuses the pane.
	copied = ""
	clickAt(m, x, y)
	if copied != "" || !m.focusDetail {
		t.Fatalf("click copied %q focus %v", copied, m.focusDetail)
	}
}

// runBatch runs a command and the commands of a batch, collecting messages
// (OSC 52 writes come back as internal messages and are ignored here).
func runBatch(cmd tea.Cmd) []tea.Msg {
	msg := cmd()
	if b, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range b {
			if c != nil {
				out = append(out, runBatch(c)...)
			}
		}
		return out
	}
	return []tea.Msg{msg}
}

func TestSelectedRowKeepsStatusColor(t *testing.T) {
	m := demoModel(true, 120, 32)
	m.th = theme.New(true, true)
	line := ""
	for _, l := range strings.Split(viewText(m), "\n") {
		if strings.Contains(ansi.Strip(l), "build-box") && strings.Contains(ansi.Strip(l), "▌") {
			line = l
		}
	}
	green := m.th.OK.Inherit(m.th.Sel).Render(pad("online", 10))
	if !strings.Contains(line, green) {
		t.Fatalf("selected row lost its status color: %q", line)
	}
}

func TestDetailScrollIsBounded(t *testing.T) {
	m := demoModel(true, 120, 32)
	m.Update(key("tab"))
	for i := 0; i < 50; i++ {
		m.Update(key("j"))
	}
	l := m.layout()
	if l.dOff != l.dMax || m.detailScroll > l.dMax {
		t.Fatalf("scroll %d off %d max %d", m.detailScroll, l.dOff, l.dMax)
	}
}

func TestEverySizeRendersWithinBounds(t *testing.T) {
	for _, setup := range []func(*Model){
		func(m *Model) {},
		func(m *Model) { m.Update(key("2")) },
		func(m *Model) { m.Update(key("3")) },
		func(m *Model) { m.Update(key("?")) },
		func(m *Model) { m.Update(key("2")); m.Update(key("K")) },
		func(m *Model) { m.Update(key("tab")) },
		func(m *Model) { m.Update(key("2")); m.Update(key("t")); m.Update(key("G")) },
	} {
		for w := 20; w <= 140; w += 7 {
			for h := 8; h <= 44; h += 5 {
				m := demoModel(true, 120, 32)
				setup(m)
				m.Update(tea.WindowSizeMsg{Width: w, Height: h})
				lines := strings.Split(viewText(m), "\n")
				if len(lines) != h {
					t.Fatalf("%dx%d: %d lines", w, h, len(lines))
				}
				for i, l := range lines {
					if lw := ansi.StringWidth(l); lw > w {
						t.Fatalf("%dx%d line %d is %d wide: %q", w, h, i, lw, ansi.Strip(l))
					}
				}
				x, y := w/2, h/2 // the mouse never panics either
				m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				m.Update(tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelDown})
				m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
			}
		}
	}
}

// A job whose command is a multi-line script (bash -lc "a\nb") stays one
// list row, the panes stay aligned, and the full script is in the details.
func TestMultilineCommandKeepsLayout(t *testing.T) {
	m := demoModel(false, 120, 32)
	m.Update(key("2"))
	m.Update(key("t"))
	m.Update(key("G"))
	if j, ok := m.selectedJob(); !ok || j.Job.Spec.ID != "j-5c4b3a2918" {
		t.Fatalf("selected %+v", j)
	}
	lines := strings.Split(viewText(m), "\n")
	if len(lines) != 32 {
		t.Fatalf("%d lines", len(lines))
	}
	l := m.layout()
	split := l.detail.X // the detail pane's left border column
	for y := l.list.Y; y < l.list.Y+l.list.H; y++ {
		r := []rune(ansi.Strip(lines[y]))
		if len(r) <= split || !strings.ContainsRune("│╭╰", r[split]) {
			t.Fatalf("row %d: detail border not at column %d: %q", y, split, string(r))
		}
	}
	text := viewText(m)
	if strings.Contains(text, "pwned") || strings.Contains(text, "\x1b]") {
		t.Fatal("terminal escape from the job's argv reached the screen")
	}
	if !strings.Contains(text, "set -u ⏎ export PATH") {
		t.Fatal("the list row does not show the script on one line")
	}
	all, _ := m.detailContent(100)
	joined := ansi.Strip(strings.Join(all, "\n"))
	if !strings.Contains(joined, "── command (5 lines)") || !strings.Contains(joined, "\nexport PATH=$HOME/.cargo/bin:$PATH\n") {
		t.Fatalf("details lack the full script:\n%s", joined)
	}
}

func fleetModel(t *testing.T) *Model {
	t.Helper()
	m := newModel()
	m.opts.RunCommand = func(context.Context, []string, io.Reader, io.Writer, io.Writer) error { return nil }
	m.whoami = &longarm.WhoAmI{Name: "laptop", Role: "admin"}
	m.refreshGen = 1
	m.Update(refreshMsg{gen: 1, nodes: []longarm.Node{
		{Name: "live", Role: "worker", State: "approved", Online: true},
		{Name: "colab-1", Role: "worker", State: "approved", Online: true, Labels: map[string]string{"provider": "colab"}},
		{Name: "waiting", Role: "worker", State: "pending"},
		{Name: "gone-1", Role: "worker", State: "revoked"},
		{Name: "gone-2", Role: "worker", State: "revoked", Labels: map[string]string{"provider": "colab"}},
	}})
	return m
}

func enabledActions(m *Model) map[string]string {
	out := map[string]string{}
	for _, a := range m.actions() {
		if a.enabled {
			out[a.id] = a.label
		}
	}
	return out
}

func TestRevokedWorkersHiddenUntilToggled(t *testing.T) {
	m := fleetModel(t)
	if got := m.ids(); len(got) != 3 {
		t.Fatalf("visible %v", got)
	}
	text := viewText(m)
	if strings.Contains(text, "gone-1") || !strings.Contains(text, "Fleet (3)") || !strings.Contains(text, "2 revoked hidden") {
		t.Fatalf("revoked shown by default:\n%s", text)
	}
	if enabledActions(m)["revoked"] != "show revoked (2)" {
		t.Fatalf("toggle: %v", enabledActions(m))
	}
	m.Update(key("t"))
	if text := viewText(m); !strings.Contains(text, "gone-1") || !strings.Contains(text, "Fleet (5)") {
		t.Fatalf("t does not show revoked:\n%s", text)
	}
	m.selected[tabFleet] = "gone-2"
	if e := enabledActions(m); e["remove"] != "" || e["run"] != "" {
		t.Fatalf("a revoked row offers %v", e)
	}
	m.Update(key("t")) // hiding again moves the selection off the revoked row
	if r, ok := m.selectedRow(); !ok || r.State == "revoked" {
		t.Fatalf("selection %+v %v", r, ok)
	}
}

func TestAllRevokedShowsHowToSeeThem(t *testing.T) {
	m := newModel()
	m.refreshGen = 1
	m.Update(refreshMsg{gen: 1, nodes: []longarm.Node{{Name: "gone", Role: "worker", State: "revoked"}}})
	if text := viewText(m); !strings.Contains(text, "No live workers.") || !strings.Contains(text, "1 revoked; t shows them") {
		t.Fatalf("empty state:\n%s", text)
	}
}

func TestFleetWriteActions(t *testing.T) {
	m := fleetModel(t)
	m.selected[tabFleet] = "live"
	e := enabledActions(m)
	if e["run"] == "" || e["colab"] == "" || e["remove"] != "revoke" || e["approve"] != "" {
		t.Fatalf("live row: %v", e)
	}
	m.Update(key("X"))
	if m.overlay != "confirm" || m.confirm.button != "Revoke" || m.confirmFocus != 1 {
		t.Fatalf("X on a worker: overlay %q %+v", m.overlay, m.confirm)
	}
	m.Update(key("esc"))
	if m.overlay != "" {
		t.Fatal("Esc did not cancel")
	}

	m.selected[tabFleet] = "colab-1"
	if e := enabledActions(m); e["remove"] != "remove Colab worker" {
		t.Fatalf("colab row: %v", e)
	}
	m.Update(key("X")) // its own reviewed flow: a terminal handoff, no modal
	if m.overlay != "" || !m.toolBusy || !strings.Contains(m.status, "Remove Colab worker colab-1") {
		t.Fatalf("X on a Colab worker: overlay %q busy %v status %q", m.overlay, m.toolBusy, m.status)
	}
	m.Update(toolDoneMsg{label: "Remove Colab worker colab-1"})

	m.selected[tabFleet] = "waiting"
	if e := enabledActions(m); e["approve"] == "" || e["run"] != "" {
		t.Fatalf("pending row (admin): %v", e)
	}
	m.whoami.Role = "user"
	if e := enabledActions(m); e["approve"] != "" {
		t.Fatalf("pending row (user): %v", e)
	}
}

func TestRunJobSelectsTheNewJob(t *testing.T) {
	m := fleetModel(t)
	m.selected[tabFleet] = "live"
	m.Update(key("n"))
	if !m.toolBusy || m.jumpSince.IsZero() {
		t.Fatalf("n: busy %v jumpSince %v", m.toolBusy, m.jumpSince)
	}
	m.Update(toolDoneMsg{label: "Run job"})
	now := time.Now()
	gen := m.refreshGen
	m.Update(refreshMsg{gen: gen, nodes: nodes("live"), jobs: []longarm.FleetJob{
		{Worker: "live", Job: longarm.Job{State: "JOB_STATE_SUCCEEDED", CreatedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), EndedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), Spec: longarm.JobSpec{ID: "old"}}},
		{Worker: "live", Job: longarm.Job{State: "JOB_STATE_RUNNING", CreatedAt: now.Format(time.RFC3339Nano), Spec: longarm.JobSpec{ID: "new"}}},
	}})
	if j, ok := m.selectedJob(); m.tab != tabJobs || !ok || j.Job.Spec.ID != "new" || !m.jumpSince.IsZero() {
		t.Fatalf("tab %d selected %+v", m.tab, j)
	}
}

func TestCanceledRunJobStaysPut(t *testing.T) {
	m := fleetModel(t)
	m.selected[tabFleet] = "live"
	m.Update(key("n"))
	m.Update(toolDoneMsg{label: "Run job", err: errString("wizard canceled")})
	m.Update(refreshMsg{gen: m.refreshGen, nodes: nodes("live"), jobs: []longarm.FleetJob{
		{Worker: "live", Job: longarm.Job{State: "JOB_STATE_RUNNING", CreatedAt: time.Now().Format(time.RFC3339Nano), Spec: longarm.JobSpec{ID: "other"}}},
	}})
	if m.tab != tabFleet {
		t.Fatal("a canceled form switched tabs")
	}
}
