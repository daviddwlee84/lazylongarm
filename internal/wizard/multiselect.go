package wizard

import (
	"context"
	"fmt"
	"io"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/daviddwlee84/lazylongarm/internal/ui/theme"
)

type MultiSpec struct {
	Title, Description string
	Choices            []Choice
	Selected           []string
	AllowEmpty, Back   bool
}

func MultiChoose(ctx context.Context, spec MultiSpec, in io.Reader, out io.Writer) ([]string, error) {
	m := newMultiPicker(spec)
	m.th = theme.New(colorFrom(ctx, out), true)
	m.query.SetStyles(inputStyles(m.th))
	result, err := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out)).Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	m = result.(*multiPicker)
	if m.back {
		return m.values(), ErrBack
	}
	if !m.submitted {
		return m.values(), ErrCanceled
	}
	return m.values(), nil
}

type multiPicker struct {
	spec                    MultiSpec
	th                      theme.Theme
	checked                 map[string]bool
	query                   textinput.Model
	search, submitted, back bool
	index, width, height    int
	err                     string
	pressed                 int
}

func newMultiPicker(spec MultiSpec) *multiPicker {
	m := &multiPicker{spec: spec, checked: map[string]bool{}, width: 80, height: 24, th: theme.New(false, true), pressed: hitNone}
	m.query = textinput.New()
	m.query.Prompt = "/ "
	m.query.Placeholder = "type to filter"
	for _, value := range spec.Selected {
		m.checked[value] = true
	}
	return m
}
func (m *multiPicker) Init() tea.Cmd { return nil }
func (m *multiPicker) rows() []Choice {
	var rows []Choice
	for _, c := range m.spec.Choices {
		if strings.Contains(strings.ToLower(c.Label+" "+c.Value), strings.ToLower(m.query.Value())) {
			rows = append(rows, c)
		}
	}
	return rows
}
func (m *multiPicker) values() []string {
	var result []string
	for _, c := range m.spec.Choices {
		if m.checked[c.Value] {
			result = append(result, c.Value)
		}
	}
	return result
}
func (m *multiPicker) submit() tea.Cmd {
	if len(m.rows()) == 0 && m.query.Value() != "" {
		m.err = "No matches; clear the filter before continuing"
		return nil
	}
	if !m.spec.AllowEmpty && len(m.values()) == 0 {
		m.err = "Select at least one item with Space"
		return nil
	}
	m.submitted = true
	return tea.Quit
}
func (m *multiPicker) toggle(i int) {
	rows := m.rows()
	if len(rows) == 0 {
		return
	}
	value := rows[min(i, len(rows)-1)].Value
	m.checked[value] = !m.checked[value]
	m.err = ""
}
func (m *multiPicker) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, msg.Width), max(1, msg.Height)
		m.query.SetWidth(max(1, m.width-8))
		m.pressed = hitNone
	case tea.PasteMsg:
		if m.search {
			var cmd tea.Cmd
			m.query, cmd = m.query.Update(msg)
			m.index = 0
			return m, cmd
		}
	case tea.MouseWheelMsg:
		switch msg.Mouse().Button {
		case tea.MouseWheelUp:
			m.index = max(0, m.index-1)
		case tea.MouseWheelDown:
			m.index = min(max(0, len(m.rows())-1), m.index+1)
		}
		m.pressed = hitNone
	case tea.MouseClickMsg:
		m.pressed = hitNone
		if msg.Mouse().Button == tea.MouseLeft {
			m.pressed = m.hitAt(msg.Mouse().X, msg.Mouse().Y)
		}
	case tea.MouseReleaseMsg:
		i := m.hitAt(msg.Mouse().X, msg.Mouse().Y)
		pressed := m.pressed
		m.pressed = hitNone
		if i != pressed || i == hitNone {
			return m, nil
		}
		switch {
		case i == hitSubmit:
			return m, m.submit()
		case i == hitBack:
			m.back = m.spec.Back
			return m, tea.Quit
		case i >= hitChoice:
			m.index = i - hitChoice
			m.toggle(m.index)
		}
	case tea.KeyPressMsg:
		m.pressed = hitNone
		key := msg.String()
		if key == "ctrl+c" {
			return m, tea.Quit
		}
		if m.search {
			switch key {
			case "esc":
				m.search = false
				m.query.Blur()
				m.query.SetValue("")
				m.index = 0
				return m, nil
			case "enter":
				m.search = false
				m.query.Blur()
				return m, nil
			case "up", "down":
				delta := 1
				if key == "up" {
					delta = -1
				}
				m.index = max(0, min(m.index+delta, len(m.rows())-1))
				return m, nil
			}
			old := m.query.Value()
			var cmd tea.Cmd
			m.query, cmd = m.query.Update(msg)
			if old != m.query.Value() {
				m.index = 0
			}
			return m, cmd
		}
		switch key {
		case "esc":
			m.back = m.spec.Back
			return m, tea.Quit
		case "enter", "ctrl+s":
			return m, m.submit()
		case "/":
			m.search = true
			return m, m.query.Focus()
		case "up", "k":
			m.index = max(0, m.index-1)
		case "down", "j":
			m.index = min(max(0, len(m.rows())-1), m.index+1)
		case "home":
			m.index = 0
		case "end", "G":
			m.index = max(0, len(m.rows())-1)
		case "space":
			m.toggle(m.index)
		}
	}
	return m, nil
}

// layout renders the screen and its click targets (shared by View and the
// mouse handler).
func (m *multiPicker) layout() ([]string, []hit) {
	t := m.th
	rows := m.rows()
	var lines []string
	for _, l := range strings.Split(ansi.Wordwrap(clean(m.spec.Title), max(10, m.width-2), " "), "\n") {
		lines = append(lines, " "+t.Accent.Render(l))
	}
	if d := clean(m.spec.Description); d != "" {
		for _, l := range strings.Split(ansi.Wordwrap(d, max(10, m.width-2), " "), "\n") {
			lines = append(lines, " "+t.Muted.Render(l))
		}
	}
	count := t.Info.Render(fmt.Sprintf("%d selected", len(m.values()))) + t.Muted.Render(fmt.Sprintf(" · %d of %d shown", len(rows), len(m.spec.Choices)))
	lines = append(lines, "", " "+count)
	if m.search {
		lines = append(lines, " "+m.query.View())
	} else if q := m.query.Value(); q != "" {
		lines = append(lines, " "+t.Muted.Render("filter: ")+clean(q)+t.Muted.Render("  (/ edits)"))
	}
	var hits []hit
	room := max(1, m.height-len(lines)-4) // error/blank, buttons, blank, keys
	start := max(0, min(m.index-room/2, len(rows)-room))
	for i := start; i < min(len(rows), start+room); i++ {
		cursor, box, st := "  ", t.Muted.Render("[ ]"), t.Text
		if m.checked[rows[i].Value] {
			box = t.OK.Render("[x]")
		}
		if i == m.index {
			cursor, st = t.Accent.Render("›")+" ", t.Bold
		}
		name, more, cut := strings.Cut(clean(rows[i].Label), " — ")
		text := st.Render(name)
		if cut {
			text += t.Muted.Render(" — " + more)
		}
		hits = append(hits, hit{0, len(lines), m.width, hitChoice + i})
		lines = append(lines, " "+cursor+box+" "+text)
	}
	if len(rows) == 0 {
		lines = append(lines, "   "+t.Warn.Render("No matching choices")+t.Muted.Render(" · Esc clears the filter"))
	}
	if len(lines) > max(0, m.height-4) {
		lines = lines[:max(0, m.height-4)]
		hits = visibleHits(hits, len(lines))
	}
	errLine := ""
	if m.err != "" {
		errLine = " " + t.Err.Render("✗ "+clean(m.err))
	}
	lines = append(lines, errLine)
	back := "Cancel Esc"
	if m.spec.Back {
		back = "Back Esc"
	}
	next := "Continue ⏎"
	hits = append(hits, hit{1, len(lines), theme.ButtonWidth(next), hitSubmit}, hit{1 + theme.ButtonWidth(next) + 2, len(lines), theme.ButtonWidth(back), hitBack})
	lines = append(lines, " "+t.PrimaryText(next, true)+"  "+t.ButtonText(back, false))
	for len(lines) < m.height-1 {
		lines = append(lines, "")
	}
	keys := []string{t.KeyHint("↑↓/jk", "move"), t.KeyHint("Space", "toggle"), t.KeyHint("/", "search"), t.KeyHint("Enter", "continue"), t.Muted.Render("click toggles")}
	lines = append(lines, " "+strings.Join(keys, t.Muted.Render(" · ")))
	return lines, hits
}

func visibleHits(hits []hit, rows int) []hit {
	var out []hit
	for _, h := range hits {
		if h.y < rows {
			out = append(out, h)
		}
	}
	return out
}

func (m *multiPicker) hitAt(x, y int) int {
	if x < 0 || y < 0 || x >= m.width || y >= m.height {
		return hitNone
	}
	_, hits := m.layout()
	for i := len(hits) - 1; i >= 0; i-- {
		h := hits[i]
		if y == h.y && x >= h.x && x < h.x+h.width {
			return h.index
		}
	}
	return hitNone
}

func (m *multiPicker) View() tea.View {
	lines, _ := m.layout()
	return terminalView(lines, m.width, m.height)
}
