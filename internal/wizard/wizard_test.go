package wizard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
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
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "ctrl+s":
		return tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}
	}
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: s}
}

func hubSpec() Spec {
	return Spec{
		Title:       "Hub on my-vps",
		Description: "Ubuntu 24.04 · linux/amd64 · 4 CPU · 7.8 GiB · sudo passwordless",
		SubmitLabel: "Review", Back: true,
		Fields: []Field{
			{Key: "name", Label: "Hub name (in lazylongarm)", Value: "my-vps", Required: true, Help: "short id used by --hub and the dashboard"},
			{Key: "address", Label: "Address nodes dial", Value: "hub.example.com", Required: true, Help: "DNS name or IP of this machine as seen by workers and clients (port is separate)"},
			{Key: "mode", Label: "Network", Kind: Select, Value: "public", Options: []Choice{{"public", "public — reachable from the Internet (domain / public IP)"}, {"private", "private — only on my network (e.g. tailnet IP)"}}},
			{Key: "port", Label: "Hub port", Value: "7443", Required: true, Help: "tcp + udp; the second STUN port is port+1"},
			{Key: "cloud", Label: "Cloud firewall", Kind: Select, Value: "none", Options: []Choice{{"azure", "Azure — open the ports in the VM's NSG with az"}, {"none", "none — I manage the firewall myself"}}},
			{Key: "enroll", Label: "Enroll this machine as admin", Kind: Toggle, Value: "true", Options: []Choice{{"true", "yes — install longarm here and join"}, {"false", "no"}}},
			{Key: "approve", Label: "Auto-approve workers", Kind: Toggle, Value: "false", Options: []Choice{{"false", "no — admin approves (owned workers join pre-approved)"}, {"true", "yes"}}},
		},
	}
}

func newTestForm(spec Spec, color bool, w, h int) *form {
	m := newForm(spec, color)
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m.Init()
	return m
}

func viewOf(v tea.View) string { return v.Content }

func TestFocusCursorAndChoiceUseDistinctMarkers(t *testing.T) {
	m := newTestForm(hubSpec(), false, 100, 30)
	m.focus(2) // Network
	out := viewOf(m.View())
	if !strings.Contains(out, "┃ Network") {
		t.Fatalf("focused field lacks the focus bar:\n%s", out)
	}
	if !strings.Contains(out, "› ● public") || !strings.Contains(out, "  ○ private") {
		t.Fatalf("choice cursor/marks missing:\n%s", out)
	}
	if strings.Contains(out, "> ") {
		t.Fatalf("the ambiguous > marker is back:\n%s", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("color off must not emit SGR sequences:\n%q", out)
	}
}

func TestArrowsOnTextFieldsMoveBetweenFields(t *testing.T) {
	m := newTestForm(hubSpec(), false, 100, 30)
	m.Update(key("down"))
	if m.index != 1 {
		t.Fatalf("down on a text field: index %d", m.index)
	}
	m.Update(key("up"))
	if m.index != 0 {
		t.Fatalf("up on a text field: index %d", m.index)
	}
	// Printable keys stay text, including j/k and q.
	for _, k := range []string{"j", "k", "q"} {
		m.Update(key(k))
	}
	if got := m.inputs[0].Value(); got != "my-vpsjkq" || m.index != 0 {
		t.Fatalf("typing changed focus or text: %q index %d", got, m.index)
	}
}

func TestSelectArrowsChooseAndEnterAdvances(t *testing.T) {
	m := newTestForm(hubSpec(), false, 100, 30)
	m.focus(2)
	m.Update(key("down"))
	if m.spec.Fields[2].Value != "private" || m.index != 2 {
		t.Fatalf("down on a select: %q index %d", m.spec.Fields[2].Value, m.index)
	}
	m.Update(key("enter"))
	if m.index != 3 {
		t.Fatalf("enter on a select should advance, index %d", m.index)
	}
}

func TestEnterOnLastFieldFocusesSubmitThenSubmits(t *testing.T) {
	m := newTestForm(hubSpec(), false, 100, 30)
	m.focus(len(m.spec.Fields) - 1)
	m.Update(key("enter"))
	if m.index != len(m.spec.Fields) {
		t.Fatalf("index %d", m.index)
	}
	if !strings.Contains(viewOf(m.View()), "[ Review ⌃S ]") {
		t.Fatalf("submit button not focused:\n%s", viewOf(m.View()))
	}
	m.Update(key("enter"))
	if !m.submitted {
		t.Fatal("enter on the submit button did not submit")
	}
}

func TestCtrlSFromFirstFieldSubmitsDraft(t *testing.T) {
	m := newTestForm(hubSpec(), false, 100, 30)
	m.Update(key("x"))
	m.Update(key("ctrl+s"))
	if !m.submitted || m.values()["name"] != "my-vpsx" {
		t.Fatalf("submitted %v values %v", m.submitted, m.values())
	}
}

func TestRequiredErrorStaysAtTheField(t *testing.T) {
	spec := hubSpec()
	spec.Fields[1].Value = ""
	m := newTestForm(spec, false, 100, 30)
	m.Update(key("ctrl+s"))
	if m.submitted || m.index != 1 {
		t.Fatalf("submitted %v index %d", m.submitted, m.index)
	}
	out := viewOf(m.View())
	if !strings.Contains(out, "┃ ✗ Address nodes dial is required") {
		t.Fatalf("error not inside the field block:\n%s", out)
	}
}

func TestSearchWithoutMatchesCannotSubmit(t *testing.T) {
	m := newTestForm(hubSpec(), false, 100, 30)
	m.focus(2)
	m.Update(key("/"))
	for _, r := range "zzz" {
		m.Update(key(string(r)))
	}
	m.Update(key("ctrl+s"))
	if m.submitted {
		t.Fatal("submitted with a filter that hides every choice")
	}
}

// clickAt finds the screen cell of the first line containing text.
func clickAt(t *testing.T, view, text string) (int, int) {
	t.Helper()
	for y, l := range strings.Split(ansi.Strip(view), "\n") {
		if x := strings.Index(l, text); x >= 0 {
			return ansi.StringWidth(l[:x]), y
		}
	}
	t.Fatalf("%q not on screen:\n%s", text, ansi.Strip(view))
	return 0, 0
}

func click(m tea.Model, x, y int) {
	m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
}

func TestMouseFocusesFieldsAndPicksChoices(t *testing.T) {
	m := newTestForm(hubSpec(), true, 100, 30)
	x, y := clickAt(t, viewOf(m.View()), "Network")
	click(m, x, y)
	if m.index != 2 {
		t.Fatalf("click on a field: index %d", m.index)
	}
	x, y = clickAt(t, viewOf(m.View()), "private")
	click(m, x, y)
	if m.spec.Fields[2].Value != "private" {
		t.Fatalf("click on a choice: %q", m.spec.Fields[2].Value)
	}
	// A press that is released elsewhere does nothing.
	bx, by := clickAt(t, viewOf(m.View()), "Review ⌃S")
	m.Update(tea.MouseClickMsg{X: bx, Y: by, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: bx, Y: by - 3, Button: tea.MouseLeft})
	if m.submitted {
		t.Fatal("release off the button submitted")
	}
	// A resize between press and release cancels the press.
	m.Update(tea.MouseClickMsg{X: bx, Y: by, Button: tea.MouseLeft})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.Update(tea.MouseReleaseMsg{X: bx, Y: by, Button: tea.MouseLeft})
	if m.submitted {
		t.Fatal("stale press submitted after a resize")
	}
	click(m, bx, by)
	if !m.submitted {
		t.Fatal("click on Review did not submit")
	}
}

func TestReviewDefaultsToBackAndButtonsAreClickable(t *testing.T) {
	r := &review{title: "Review · Hub", body: "x", width: 80, height: 20, back: true, th: theme.New(false, true)}
	out := viewOf(r.View())
	if !strings.Contains(out, "[ Back ]") || strings.Contains(out, "[ Apply ]") {
		t.Fatalf("review must focus Back first:\n%s", out)
	}
	r.Update(key("enter"))
	if r.accepted {
		t.Fatal("enter on the default focus applied")
	}
	r = &review{title: "Review · Hub", body: "x", width: 80, height: 20, back: true, th: theme.New(true, true)}
	x, y := clickAt(t, viewOf(r.View()), "Apply")
	click(r, x, y)
	if !r.accepted {
		t.Fatal("click on Apply did not accept")
	}
}

func TestMultiPickerClickToggles(t *testing.T) {
	m := newMultiPicker(MultiSpec{Title: "Register which machines?", Choices: []Choice{{"a", "worker-1 — box2"}, {"b", "worker-2 — box3"}}})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	x, y := clickAt(t, viewOf(m.View()), "worker-2")
	click(m, x, y)
	if got := m.values(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("values %v", got)
	}
}

func TestWideTextKeepsWidth(t *testing.T) {
	spec := hubSpec()
	spec.Fields[0].Value = "專案-é-👩🏽‍💻"
	m := newTestForm(spec, true, 60, 20)
	m.focus(2)
	for i, l := range strings.Split(viewOf(m.View()), "\n") {
		if w := ansi.StringWidth(l); w > 60 {
			t.Fatalf("line %d is %d cells wide: %q", i, w, ansi.Strip(l))
		}
	}
}

// TestSnapshots writes rendered wizard screens for visual review when
// LAZYLONGARM_SNAPSHOT_DIR is set.
func TestSnapshots(t *testing.T) {
	dir := os.Getenv("LAZYLONGARM_SNAPSHOT_DIR")
	if dir == "" {
		t.Skip("set LAZYLONGARM_SNAPSHOT_DIR to write wizard snapshots")
	}
	body := `Hub on my-vps

  host     my-vps
  address  hub.example.com:7443
  version  v0.9.2

 1. Install longarm-hub v0.9.2
      # [my-vps] install -d -m 0755 /opt/longarm && curl -fsSL https://github.com/lubobill1990/longarm/releases/download/v0.9.2/longarm-hub_linux_amd64.tar.gz | tar -xz -C /opt/longarm
 2. Write the hub config
      # [my-vps] install -m 0600 /dev/stdin /etc/longarm/hub.toml
 3. Start longarm-hub.service
      # [my-vps] systemctl enable --now longarm-hub
 4. Enroll this machine as admin
        [local] longarm join hub.example.com:7443 --code ••••••

  ! The hub's UDP port 7444 must be reachable for direct paths.
`
	for _, size := range [][2]int{{100, 30}, {80, 24}} {
		for _, color := range []bool{true, false} {
			suffix := ""
			if !color {
				suffix = "-nocolor"
			}
			write := func(name string, v tea.View) {
				path := filepath.Join(dir, fmt.Sprintf("wiz-%s-%dx%d%s.ansi", name, size[0], size[1], suffix))
				if err := os.WriteFile(path, []byte(v.Content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			m := newTestForm(hubSpec(), color, size[0], size[1])
			m.focus(2)
			write("form-select", m.View())
			m = newTestForm(hubSpec(), color, size[0], size[1])
			m.focus(1)
			m.inputs[1].SetValue("")
			m.Update(key("ctrl+s"))
			write("form-error", m.View())
			var hosts []Choice
			for i := 1; i <= 14; i++ {
				hosts = append(hosts, Choice{fmt.Sprintf("host-%d", i), fmt.Sprintf("worker-%d — user@10.0.0.%d", i, i)})
			}
			c := newTestForm(Spec{Title: "Where should longarm-hub run?", Description: "Pick an SSH host; lazylongarm probes it before anything changes. Type / to search.", SubmitLabel: "Probe", Fields: []Field{{Key: "host", Kind: Select, Options: hosts, Required: true}}}, color, size[0], size[1])
			c.Update(key("down"))
			c.Update(key("down"))
			write("choose", c.View())
			r := &review{title: "Review · Hub on my-vps", body: body, width: size[0], height: size[1], back: true, th: theme.New(color, true)}
			write("review", r.View())
			mp := newMultiPicker(MultiSpec{Title: "Register which machines in hc?", Description: "Machines already registered are unchecked. Space toggles.", Choices: []Choice{{"a", "worker-1 — box2"}, {"b", "build-box — build-box"}, {"c", "gpu-01 — gpu-01"}}, Selected: []string{"a", "c"}})
			mp.th = theme.New(color, true)
			mp.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			mp.Update(key("down"))
			write("multi", mp.View())
		}
	}
}

func TestEverySizeRendersWithinBounds(t *testing.T) {
	for w := 12; w <= 140; w += 8 {
		for h := 4; h <= 40; h += 4 {
			for focus := 0; focus <= len(hubSpec().Fields); focus++ {
				m := newTestForm(hubSpec(), true, w, h)
				m.focus(focus)
				lines := strings.Split(viewOf(m.View()), "\n")
				if len(lines) > h {
					t.Fatalf("%dx%d focus %d: %d lines", w, h, focus, len(lines))
				}
				for i, l := range lines {
					if lw := ansi.StringWidth(l); lw > w {
						t.Fatalf("%dx%d line %d is %d wide", w, h, i, lw)
					}
				}
				m.Update(tea.MouseClickMsg{X: w / 2, Y: h / 2, Button: tea.MouseLeft})
				m.Update(tea.MouseReleaseMsg{X: w / 2, Y: h / 2, Button: tea.MouseLeft})
			}
			r := &review{title: "Review", body: strings.Repeat("a long line of plan text with words\n", 30), width: w, height: h, th: theme.New(true, true)}
			if n := len(strings.Split(viewOf(r.View()), "\n")); n > h {
				t.Fatalf("review %dx%d: %d lines", w, h, n)
			}
		}
	}
}
