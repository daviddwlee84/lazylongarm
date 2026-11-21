package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
)

// Mouse handling. Every event is hit-tested against a fresh m.layout(), the
// same calculation View renders from. Tabs and rows act on press; footer
// actions and modal buttons act on release, only when the release lands on
// the same target and nothing (size, tab, overlay, selection) changed since
// the press. A dragged range in the right pane is copied on release.

// press is a pending footer/button click.
type press struct {
	z   zone
	sig string
}

// drag is a line range being selected (or just copied) in the right pane.
type drag struct {
	anchor, cur int // content line indexes
	moved, done bool
}

func (d *drag) span() (int, int) { return min(d.anchor, d.cur), max(d.anchor, d.cur) }
func (d *drag) covers(n int) bool {
	if !d.moved {
		return false
	}
	lo, hi := d.span()
	return n >= lo && n <= hi
}

// sig identifies the state a press was made in; any change cancels it.
func (m *Model) sig() string {
	return fmt.Sprint(m.width, "x", m.height, m.tab, m.overlay, m.selected[m.tab], m.focusDetail, m.toolBusy)
}

func (m *Model) mouseMsg(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if !m.mouse {
		return m, nil
	}
	mo := msg.Mouse()
	l := m.layout()
	switch msg.(type) {
	case tea.MouseWheelMsg:
		return m, m.wheel(l, mo)
	case tea.MouseClickMsg:
		m.press = nil
		if mo.Button != tea.MouseLeft {
			return m, nil
		}
		return m, m.click(l, mo.X, mo.Y)
	case tea.MouseMotionMsg:
		if d := m.drag; d != nil && !d.done {
			in := l.detail.Inner()
			if !d.moved { // freeze the viewport while selecting
				m.detailScroll, m.detailFollow = l.dOff, false
			}
			d.moved = true
			switch {
			case mo.Y < in.Y+l.dPin && l.dOff > 0 && d.anchor >= l.dPin:
				m.detailScroll = l.dOff - 1
			case mo.Y >= in.Y+in.H && l.dOff < l.dMax:
				m.detailScroll = l.dOff + 1
			}
			d.cur = m.layout().detailLineAt(mo.Y)
		}
		return m, nil
	case tea.MouseReleaseMsg:
		if d := m.drag; d != nil && !d.done {
			if !d.moved {
				m.drag = nil
				return m, nil
			}
			d.done = true
			lo, hi := d.span()
			var lines []string
			for i := lo; i <= hi && i < len(l.dLines); i++ {
				lines = append(lines, strings.TrimRight(ansi.Strip(l.dLines[i]), " "))
			}
			return m, m.copyText(plural(len(lines), "line"), strings.Join(lines, "\n"))
		}
		p := m.press
		m.press = nil
		if p == nil {
			return m, nil
		}
		z := l.hit(mo.X, mo.Y)
		if z.kind != p.z.kind || z.i != p.z.i || z.id != p.z.id || m.sig() != p.sig {
			return m, nil
		}
		switch z.kind {
		case zButton:
			return m, m.confirmChoose(z.i)
		case zHelp:
			m.overlay, m.helpScroll = "help", 0
		case zAction:
			for _, a := range m.actions() {
				if a.id == z.id && a.enabled {
					return m, a.run()
				}
			}
		}
	}
	return m, nil
}

func (m *Model) click(l layout, x, y int) tea.Cmd {
	z := l.hit(x, y)
	if m.overlay != "" { // the modal owns every click; nothing behind it reacts
		switch {
		case z.kind == zButton:
			m.press = &press{z: z, sig: m.sig()}
		case z.kind == zNone && m.overlay == "help":
			m.overlay = ""
		}
		return nil
	}
	m.drag = nil
	switch z.kind {
	case zTab:
		return m.switchTab(z.i)
	case zRow:
		m.focusDetail = false
		if m.selected[m.tab] != z.id {
			m.selected[m.tab] = z.id
			return m.afterMove()
		}
	case zList:
		m.focusDetail = false
	case zDetail:
		m.focusDetail = true
		if l.detail.Inner().Has(x, y) && len(l.dLines) > 0 {
			n := l.detailLineAt(y)
			m.drag = &drag{anchor: n, cur: n}
		}
	case zAction, zHelp:
		m.press = &press{z: z, sig: m.sig()}
	}
	return nil
}

func (m *Model) wheel(l layout, mo tea.Mouse) tea.Cmd {
	delta := 0
	switch mo.Button {
	case tea.MouseWheelUp:
		delta = -1
	case tea.MouseWheelDown:
		delta = 1
	}
	if delta == 0 {
		return nil
	}
	switch m.overlay {
	case "help":
		m.scrollHelp(l, 3*delta)
		return nil
	case "":
	default:
		return nil
	}
	switch l.hit(mo.X, mo.Y).kind {
	case zDetail:
		m.scrollDetail(l, 3*delta)
	case zList, zRow:
		if m.tab == tabHub {
			return nil
		}
		m.move(delta)
		return m.afterMove()
	}
	return nil
}

// scrollDetail moves the right pane by delta lines. On the Jobs tab, reaching
// the bottom resumes following the log tail.
func (m *Model) scrollDetail(l layout, delta int) {
	n := max(0, min(l.dOff+delta, l.dMax))
	m.detailScroll = n
	m.detailFollow = m.tab == tabJobs && n == l.dMax
}

func (m *Model) scrollHelp(l layout, delta int) {
	maxOff := max(0, len(l.modalLines)-(l.modal.H-2))
	m.helpScroll = max(0, min(l.helpOff+delta, maxOff))
}

type copiedMsg struct {
	what string
	err  error
}

// copyText puts text on the clipboard through the terminal (OSC 52, works
// over SSH where the terminal supports it) and the local clipboard.
func (m *Model) copyText(what, text string) tea.Cmd {
	if strings.TrimSpace(text) == "" {
		m.setStatus("warn", "nothing to copy")
		return nil
	}
	local := m.opts.Clipboard
	return tea.Batch(tea.SetClipboard(text), func() tea.Msg {
		var err error
		if local != nil {
			err = local(text)
		}
		return copiedMsg{what: what, err: err}
	})
}

// yank copies the whole log, the details or the open panel URL.
func (m *Model) yank() tea.Cmd {
	switch m.tab {
	case tabJobs:
		j, ok := m.selectedJob()
		if !ok {
			return nil
		}
		text := strings.TrimRight(connection.Sanitize(m.logs[j.Job.Spec.ID].text), "\n")
		return m.copyText("the log of "+short(j.Job.Spec.ID)+" ("+plural(strings.Count(text, "\n")+1, "line")+")", text)
	case tabHub:
		if u := m.PanelURL(); u != "" {
			return m.copyText("the panel URL", u)
		}
	}
	lines, _ := m.detailContent(1 << 16)
	if m.tab == tabHub && m.hub != nil {
		lines = append(m.hubInfoLines(), append([]string{""}, lines...)...)
	}
	for i := range lines {
		lines[i] = strings.TrimRight(ansi.Strip(lines[i]), " ")
	}
	return m.copyText("the details", strings.Join(lines, "\n"))
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
