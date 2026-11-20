package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
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
