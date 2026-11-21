// Package wizard provides terminal forms shared by CLI commands and dashboard
// handoffs. Forms collect drafts; callers own domain validation and execution.
//
// A form is one column of fields. Unfocused fields collapse to "label value";
// the focused one expands behind an accent bar with its help text and its
// editor (an input, or a list of choices with a › cursor and ●/○ marks).
// Focus, the option cursor and the chosen value each have their own marker,
// so the screen reads without color too.
package wizard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/daviddwlee84/lazylongarm/internal/ui/theme"
	"golang.org/x/term"
)

type Kind uint8

const (
	Text Kind = iota
	Secret
	Multiline
	Select
	Toggle
)

type Choice struct{ Value, Label string }
type Field struct {
	Key, Label, Value, Help string
	Kind                    Kind
	Options                 []Choice
	Required                bool
}
type Spec struct {
	Title, Description, SubmitLabel string
	Fields                          []Field
	Back                            bool
}

var ErrCanceled = errors.New("wizard canceled")
var ErrBack = errors.New("wizard back")

type colorKey struct{}

// WithColor fixes whether forms started with ctx use color (the CLI's
// --color decision). Without it, forms use color when NO_COLOR is unset and
// their output is a terminal.
func WithColor(ctx context.Context, on bool) context.Context {
	return context.WithValue(ctx, colorKey{}, on)
}

func colorFrom(ctx context.Context, out io.Writer) bool {
	if on, ok := ctx.Value(colorKey{}).(bool); ok {
		return on
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := out.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func Edit(ctx context.Context, spec Spec, in io.Reader, out io.Writer) (map[string]string, error) {
	values, err := EditDraft(ctx, spec, in, out)
	if err != nil {
		return nil, err
	}
	return values, nil
}

// EditDraft retains the current answers when Back is requested. Existing Edit
// callers keep their cancellation contract and never receive a canceled draft.
func EditDraft(ctx context.Context, spec Spec, in io.Reader, out io.Writer) (map[string]string, error) {
	if len(spec.Fields) == 0 {
		return nil, errors.New("wizard has no fields")
	}
	m := newForm(spec, colorFrom(ctx, out))
	seen := map[string]bool{}
	for _, f := range m.spec.Fields {
		if f.Key == "" || seen[f.Key] {
			return nil, errors.New("wizard fields require distinct keys")
		}
		seen[f.Key] = true
		if f.Kind == Select && len(f.Options) == 0 {
			return nil, errors.New("wizard selection has no choices")
		}
	}
	result, err := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out)).Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	form := result.(*form)
	if form.back {
		return form.values(), ErrBack
	}
	if !form.submitted {
		return form.values(), ErrCanceled
	}
	return form.values(), nil
}

func Choose(ctx context.Context, title string, choices []Choice, in io.Reader, out io.Writer) (string, error) {
	values, err := Edit(ctx, Spec{Title: title, SubmitLabel: "Select", Fields: []Field{{Key: "choice", Kind: Select, Options: choices, Required: true}}}, in, out)
	return values["choice"], err
}

func Confirm(ctx context.Context, title, body string, in io.Reader, out io.Writer) (bool, error) {
	return confirm(ctx, title, body, false, in, out)
}

// ConfirmBack labels the negative review action as Back for staged wizards.
func ConfirmBack(ctx context.Context, title, body string, in io.Reader, out io.Writer) (bool, error) {
	return confirm(ctx, title, body, true, in, out)
}
func confirm(ctx context.Context, title, body string, back bool, in io.Reader, out io.Writer) (bool, error) {
	m := &review{title: title, body: body, width: 80, height: 24, back: back, th: theme.New(colorFrom(ctx, out), true)}
	result, err := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out)).Run()
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		return false, err
	}
	review := result.(*review)
	if review.interrupted {
		return false, ErrCanceled
	}
	return review.accepted, nil
}

// Pause keeps a terminal handoff's printed result visible before the dashboard
// reacquires the terminal. It doesn't enter the alternate screen or parse input
// outside Bubble Tea's cancellable terminal reader.
func Pause(ctx context.Context, message string, in io.Reader, out io.Writer) error {
	_, err := tea.NewProgram(pause{message: message, th: theme.New(colorFrom(ctx, out), true)}, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out)).Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

type pause struct {
	message string
	th      theme.Theme
}

func (p pause) Init() tea.Cmd { return nil }
func (p pause) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "enter", "esc", "ctrl+c":
			return p, tea.Quit
		}
	}
	return p, nil
}
func (p pause) View() tea.View {
	msg := clean(p.message)
	switch {
	case msg == "Finished":
		msg = p.th.OK.Render("✓ " + msg)
	case strings.HasPrefix(msg, "Did not") || strings.HasPrefix(msg, "Cannot"):
		msg = p.th.Err.Render("✗ " + msg)
	}
	return tea.NewView(msg + p.th.Muted.Render(" · ") + p.th.KeyHint("Enter", "/") + " " + p.th.KeyHint("Esc", "to return"))
}

// hit targets: a field index, or one of these.
const (
	hitNone   = -2
	hitSubmit = -1
	hitBack   = -3
	hitChoice = 100000 // + index into the visible (filtered) choices
)

type hit struct{ x, y, width, index int }
type form struct {
	spec          Spec
	th            theme.Theme
	inputs        []textinput.Model
	areas         []textarea.Model
	index         int // a field, or len(Fields) for the submit button
	width, height int
	err           string
	submitted     bool
	back          bool
	searching     bool
	search        textinput.Model
	pressed       int
}

func newForm(spec Spec, color bool) *form {
	// Own the draft, including slices, so cancellation cannot mutate a caller.
	spec.Fields = append([]Field(nil), spec.Fields...)
	m := &form{spec: spec, width: 80, height: 24, pressed: hitNone, th: theme.New(color, true)}
	m.search = textinput.New()
	m.search.Prompt = "/ "
	m.search.Placeholder = "type to filter"
	m.search.SetStyles(inputStyles(m.th))
	for i := range m.spec.Fields {
		f := &m.spec.Fields[i]
		f.Options = append([]Choice(nil), f.Options...)
		if f.Kind == Toggle && len(f.Options) == 0 {
			f.Options = []Choice{{"false", "Off"}, {"true", "On"}}
		}
		if len(f.Options) > 0 && f.Value == "" {
			f.Value = f.Options[0].Value
		}
		input := textinput.New()
		input.Prompt = "› "
		input.CharLimit = 65536
		input.SetValue(f.Value)
		input.SetStyles(inputStyles(m.th))
		if f.Kind == Secret {
			input.EchoMode = textinput.EchoPassword
			input.EchoCharacter = '•'
		}
		area := textarea.New()
		area.Prompt = ""
		area.ShowLineNumbers = false
		area.SetValue(f.Value)
		area.CharLimit = 1 << 20
		area.SetHeight(6)
		m.inputs = append(m.inputs, input)
		m.areas = append(m.areas, area)
	}
	return m
}

func inputStyles(th theme.Theme) textinput.Styles {
	s := textinput.DefaultStyles(true)
	plain := lipgloss.NewStyle()
	s.Focused = textinput.StyleState{Text: plain, Placeholder: th.Muted, Suggestion: th.Muted, Prompt: th.Accent}
	s.Blurred = textinput.StyleState{Text: plain, Placeholder: th.Muted, Suggestion: th.Muted, Prompt: th.Muted}
	return s
}

func (m *form) Init() tea.Cmd { return m.focus(0) }
func (m *form) sync() {
	if m.index >= len(m.spec.Fields) {
		return
	}
	f := &m.spec.Fields[m.index]
	if f.Kind == Multiline {
		f.Value = m.areas[m.index].Value()
	} else if f.Kind == Text || f.Kind == Secret {
		f.Value = m.inputs[m.index].Value()
	}
}
func (m *form) values() map[string]string {
	m.sync()
	values := map[string]string{}
	for _, field := range m.spec.Fields {
		values[field.Key] = field.Value
	}
	return values
}
func (m *form) focus(index int) tea.Cmd {
	m.sync()
	for i := range m.inputs {
		m.inputs[i].Blur()
		m.areas[i].Blur()
	}
	m.search.Blur()
	m.searching = false
	m.search.SetValue("")
	m.index = max(0, min(index, len(m.spec.Fields)))
	if m.index == len(m.spec.Fields) {
		return nil
	}
	m.resize()
	if m.spec.Fields[m.index].Kind == Multiline {
		return m.areas[m.index].Focus()
	}
	if m.spec.Fields[m.index].Kind == Text || m.spec.Fields[m.index].Kind == Secret {
		return m.inputs[m.index].Focus()
	}
	return nil
}
func (m *form) resize() {
	for i := range m.inputs {
		m.inputs[i].SetWidth(max(1, m.width-8))
		m.areas[i].SetWidth(max(1, m.width-6))
		m.areas[i].SetHeight(max(1, min(7, m.height-12)))
	}
	m.search.SetWidth(max(1, m.width-10))
}
func (m *form) submit() tea.Cmd {
	m.sync()
	if m.searching && len(m.choices()) == 0 {
		m.err = "No matching choices; clear the search before selecting"
		return nil
	}
	for i, field := range m.spec.Fields {
		if field.Required && strings.TrimSpace(field.Value) == "" {
			m.err = m.fieldName(i) + " is required"
			return m.focus(i)
		}
		if field.Kind == Select || field.Kind == Toggle {
			valid := false
			for _, c := range field.Options {
				valid = valid || c.Value == field.Value
			}
			if !valid {
				m.err = "Choose a value for " + m.fieldName(i)
				return m.focus(i)
			}
		}
	}
	m.submitted = true
	return tea.Quit
}
func (m *form) fieldName(i int) string {
	if l := clean(m.spec.Fields[i].Label); l != "" {
		return l
	}
	return "A choice"
}
func (m *form) choices() []Choice {
	if m.index >= len(m.spec.Fields) {
		return nil
	}
	var choices []Choice
	query := strings.ToLower(m.search.Value())
	for _, choice := range m.spec.Fields[m.index].Options {
		if strings.Contains(strings.ToLower(choice.Label+" "+choice.Value), query) {
			choices = append(choices, choice)
		}
	}
	return choices
}
func (m *form) moveChoice(delta int) {
	choices := m.choices()
	if len(choices) == 0 {
		return
	}
	f := &m.spec.Fields[m.index]
	index := 0
	for i, choice := range choices {
		if choice.Value == f.Value {
			index = i
		}
	}
	f.Value = choices[max(0, min(index+delta, len(choices)-1))].Value
	m.err = ""
}
func (m *form) isChoice() bool {
	if m.index >= len(m.spec.Fields) {
		return false
	}
	k := m.spec.Fields[m.index].Kind
	return k == Select || k == Toggle
}

func (m *form) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, msg.Width), max(1, msg.Height)
		m.pressed = hitNone
		m.resize()
		return m, nil
	case tea.MouseWheelMsg:
		delta := map[tea.MouseButton]int{tea.MouseWheelUp: -1, tea.MouseWheelDown: 1}[msg.Mouse().Button]
		if delta == 0 {
			return m, nil
		}
		m.pressed = hitNone
		if m.isChoice() {
			m.moveChoice(delta)
			return m, nil
		}
		return m, m.focus(max(0, min(m.index+delta, len(m.spec.Fields))))
	case tea.MouseClickMsg:
		if msg.Mouse().Button == tea.MouseLeft {
			m.pressed = m.hit(msg.Mouse().X, msg.Mouse().Y)
		}
		return m, nil
	case tea.MouseReleaseMsg:
		i := m.hit(msg.Mouse().X, msg.Mouse().Y)
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
			choices := m.choices()
			if index := i - hitChoice; index < len(choices) {
				m.spec.Fields[m.index].Value = choices[index].Value
				m.err = ""
			}
			return m, nil
		}
		return m, m.focus(i)
	case tea.KeyPressMsg:
		m.pressed = hitNone
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			if m.searching {
				m.searching = false
				m.search.Blur()
				m.search.SetValue("")
				return m, nil
			}
			m.back = m.spec.Back
			return m, tea.Quit
		case "ctrl+s":
			return m, m.submit()
		case "tab":
			return m, m.focus((m.index + 1) % (len(m.spec.Fields) + 1))
		case "shift+tab":
			return m, m.focus((m.index + len(m.spec.Fields)) % (len(m.spec.Fields) + 1))
		}
		if m.index == len(m.spec.Fields) { // the submit button
			switch msg.String() {
			case "enter", "space":
				return m, m.submit()
			case "up", "k", "shift+tab":
				return m, m.focus(m.index - 1)
			}
			return m, nil
		}
		f := &m.spec.Fields[m.index]
		if f.Kind == Select || f.Kind == Toggle {
			switch msg.String() {
			case "up", "down":
				if msg.String() == "up" {
					m.moveChoice(-1)
				} else {
					m.moveChoice(1)
				}
				return m, nil
			case "enter":
				if m.searching {
					if len(m.choices()) == 0 {
						m.err = "No matching choices"
						return m, nil
					}
					m.searching = false
					m.search.Blur()
					m.search.SetValue("")
					return m, nil
				}
				if len(m.spec.Fields) == 1 {
					return m, m.submit()
				}
				return m, m.focus(m.index + 1)
			}
			if !m.searching {
				switch msg.String() {
				case "k", "left", "h":
					m.moveChoice(-1)
				case "j", "right", "l", "space":
					m.moveChoice(1)
				case "/":
					m.searching = true
					return m, m.search.Focus()
				}
				return m, nil
			}
		} else if f.Kind != Multiline {
			switch msg.String() {
			case "enter", "down":
				return m, m.focus(m.index + 1)
			case "up":
				return m, m.focus(m.index - 1)
			}
		}
	}
	var cmd tea.Cmd
	if m.searching {
		m.search, cmd = m.search.Update(msg)
		choices := m.choices()
		if len(choices) > 0 {
			found := false
			for _, c := range choices {
				found = found || c.Value == m.spec.Fields[m.index].Value
			}
			if !found {
				m.spec.Fields[m.index].Value = choices[0].Value
			}
		}
	} else if m.index < len(m.spec.Fields) {
		if m.spec.Fields[m.index].Kind == Multiline {
			m.areas[m.index], cmd = m.areas[m.index].Update(msg)
		} else if m.spec.Fields[m.index].Kind == Text || m.spec.Fields[m.index].Kind == Secret {
			m.inputs[m.index], cmd = m.inputs[m.index].Update(msg)
		}
	}
	return m, cmd
}

// row is one rendered line and, optionally, what clicking it does.
type row struct {
	text string
	hit  int // hitNone when inert
}

// fieldRows renders field i: one summary line, or the expanded focused block.
func (m *form) fieldRows(i, labelW, maxChoices int) []row {
	t := m.th
	f := m.spec.Fields[i]
	label := clean(f.Label)
	req := ""
	if f.Required {
		req = t.Muted.Render(" *")
	}
	if i != m.index {
		gap := labelW + 3 - ansi.StringWidth(label) - ansi.StringWidth(ansi.Strip(req))
		return []row{{text: "  " + t.Text.Render(label) + req + strings.Repeat(" ", max(1, gap)) + m.summary(i), hit: i}}
	}
	bar := t.Accent.Render("┃") + " "
	var rows []row
	if label != "" {
		rows = append(rows, row{text: bar + t.Accent.Render(label) + req, hit: hitNone})
	}
	if h := clean(f.Help); h != "" {
		for _, l := range strings.Split(ansi.Wordwrap(h, max(10, m.width-4), " "), "\n") {
			rows = append(rows, row{text: bar + t.Muted.Render(l), hit: hitNone})
		}
	}
	switch f.Kind {
	case Text, Secret:
		rows = append(rows, row{text: bar + m.inputs[i].View(), hit: hitNone})
	case Multiline:
		for _, l := range strings.Split(m.areas[i].View(), "\n") {
			rows = append(rows, row{text: bar + l, hit: hitNone})
		}
	case Select, Toggle:
		if m.searching {
			rows = append(rows, row{text: bar + m.search.View(), hit: hitNone})
		}
		choices := m.choices()
		selected := 0
		for k, c := range choices {
			if c.Value == f.Value {
				selected = k
			}
		}
		n := min(len(choices), max(1, maxChoices))
		first := max(0, min(selected-n/2, len(choices)-n))
		if first > 0 {
			rows = append(rows, row{text: bar + t.Muted.Render(fmt.Sprintf("  ↑ %d more", first)), hit: hitNone})
		}
		for k := first; k < first+n; k++ {
			c := choices[k]
			cursor, mark, st := "  ", t.Muted.Render("○"), t.Text
			if c.Value == f.Value {
				cursor, mark, st = t.Accent.Render("›")+" ", t.Accent.Render("●"), t.Bold
			}
			name, more, cut := strings.Cut(clean(c.Label), " — ")
			text := st.Render(name)
			if cut {
				text += t.Muted.Render(" — " + more)
			}
			rows = append(rows, row{text: bar + cursor + mark + " " + text, hit: hitChoice + k})
		}
		if rest := len(choices) - first - n; rest > 0 {
			rows = append(rows, row{text: bar + t.Muted.Render(fmt.Sprintf("  ↓ %d more · / to search", rest)), hit: hitNone})
		}
		if len(choices) == 0 {
			rows = append(rows, row{text: bar + t.Warn.Render("No matching choices") + t.Muted.Render(" · Esc clears the search"), hit: hitNone})
		}
	}
	if m.err != "" {
		rows = append(rows, row{text: bar + t.Err.Render("✗ "+clean(m.err)), hit: hitNone})
	}
	return rows
}

// summary is a collapsed field's value.
func (m *form) summary(i int) string {
	t := m.th
	f := m.spec.Fields[i]
	value := f.Value
	switch f.Kind {
	case Secret:
		if value != "" {
			value = "••••••••"
		}
	case Multiline:
		if first, _, more := strings.Cut(strings.TrimSpace(value), "\n"); more {
			value = first + " …"
		}
	case Select, Toggle:
		for _, c := range f.Options {
			if c.Value == value {
				name, _, _ := strings.Cut(clean(c.Label), " — ")
				return t.Bold.Render(name)
			}
		}
	}
	if strings.TrimSpace(value) == "" {
		if f.Required {
			return t.Err.Render("required")
		}
		return t.Muted.Render("(empty)")
	}
	return t.Bold.Render(clean(value))
}

// layout renders the whole screen and the click targets of its visible lines.
// View and the mouse handler both call it, so hits match what is drawn.
func (m *form) layout() ([]string, []hit) {
	t := m.th
	var lines []string
	for _, l := range strings.Split(ansi.Wordwrap(clean(m.spec.Title), max(10, m.width-2), " "), "\n") {
		lines = append(lines, " "+t.Accent.Render(l))
	}
	if d := clean(m.spec.Description); d != "" {
		for _, l := range strings.Split(ansi.Wordwrap(d, max(10, m.width-2), " "), "\n") {
			lines = append(lines, " "+t.Muted.Render(l))
		}
	}
	lines = append(lines, "")
	var hits []hit

	labelW := 0
	for _, f := range m.spec.Fields {
		labelW = max(labelW, ansi.StringWidth(clean(f.Label)))
	}
	labelW = min(labelW, max(8, m.width/2))
	footer := 3 // blank, buttons, keys
	room := max(1, m.height-len(lines)-footer)
	if m.height < len(lines)+footer+1 { // tiny terminal: drop the description
		lines = lines[:1]
		room = max(1, m.height-1-footer)
	}

	// Every field's rows; the focused one gets as many choice rows as fit.
	collapsed := len(m.spec.Fields) - 1
	var all []row
	focusTop, focusLen := 0, 0
	for i := range m.spec.Fields {
		rows := m.fieldRows(i, labelW, room-collapsed-4)
		if i == m.index {
			focusTop, focusLen = len(all), len(rows)
		}
		all = append(all, rows...)
	}
	start := 0
	if len(all) > room {
		start = max(0, min(focusTop-(room-focusLen)/2, len(all)-room))
		if m.index == len(m.spec.Fields) {
			start = len(all) - room
		}
	}
	end := min(len(all), start+room)
	for k := start; k < end; k++ {
		r := all[k]
		switch {
		case k == start && start > 0:
			r = row{text: t.Muted.Render(fmt.Sprintf("  ↑ %d more above", start+1)), hit: hitNone}
		case k == end-1 && end < len(all):
			r = row{text: t.Muted.Render(fmt.Sprintf("  ↓ %d more below", len(all)-end+1)), hit: hitNone}
		}
		if r.hit != hitNone {
			hits = append(hits, hit{0, len(lines), m.width, r.hit})
		}
		lines = append(lines, r.text)
	}
	lines = append(lines, "")

	label := m.spec.SubmitLabel
	if label == "" {
		label = "Review"
	}
	submit := clean(label) + " ⌃S"
	back := "Cancel Esc"
	if m.spec.Back {
		back = "Back Esc"
	}
	x := 1
	buttons := " " + t.PrimaryText(submit, m.index == len(m.spec.Fields))
	hits = append(hits, hit{x, len(lines), theme.ButtonWidth(submit), hitSubmit})
	x += theme.ButtonWidth(submit) + 2
	buttons += "  " + t.ButtonText(back, false)
	hits = append(hits, hit{x, len(lines), theme.ButtonWidth(back), hitBack})
	if n := len(m.spec.Fields); n > 1 {
		step := fmt.Sprintf("%d / %d", min(m.index+1, n), n)
		if m.index == n {
			step = "ready"
		}
		if gap := m.width - ansi.StringWidth(buttons) - ansi.StringWidth(step) - 1; gap > 1 {
			buttons += strings.Repeat(" ", gap) + t.Muted.Render(step)
		}
	}
	lines = append(lines, buttons)
	for len(lines) < m.height-1 {
		lines = append(lines, "")
	}
	lines = append(lines, " "+m.keyHints())
	return lines, hits
}

func (m *form) keyHints() string {
	t := m.th
	var hints []string
	switch {
	case m.index == len(m.spec.Fields):
		hints = []string{t.KeyHint("Enter", "submit"), t.KeyHint("↑/Shift+Tab", "back to the fields")}
	case m.searching:
		hints = []string{t.KeyHint("type", "to filter"), t.KeyHint("↑↓", "choose"), t.KeyHint("Enter", "keep"), t.KeyHint("Esc", "clear")}
	case m.isChoice():
		next := "next"
		if len(m.spec.Fields) == 1 {
			next = "select"
		}
		hints = []string{t.KeyHint("↑↓", "choose"), t.KeyHint("Enter", next), t.KeyHint("Tab", "field"), t.KeyHint("/", "search")}
	case m.spec.Fields[m.index].Kind == Multiline:
		hints = []string{t.KeyHint("Tab", "next field"), t.KeyHint("Ctrl+S", "done")}
	default:
		hints = []string{t.KeyHint("Enter/↓", "next"), t.KeyHint("↑", "previous"), t.KeyHint("Tab", "field")}
	}
	hints = append(hints, t.Muted.Render("click works too"))
	return strings.Join(hints, t.Muted.Render(" · "))
}

func (m *form) hit(x, y int) int {
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
func (m *form) View() tea.View {
	lines, _ := m.layout()
	return terminalView(lines, m.width, m.height)
}

type review struct {
	title, body           string
	th                    theme.Theme
	width, height, offset int
	acceptFocus, accepted bool
	interrupted           bool
	back                  bool
	pressed               int
}

func (m *review) Init() tea.Cmd { m.pressed = -1; return nil }
func (m *review) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, msg.Width), max(1, msg.Height)
		m.pressed = -1
	case tea.KeyPressMsg:
		m.pressed = -1
		switch msg.String() {
		case "ctrl+c":
			m.interrupted = true
			return m, tea.Quit
		case "esc":
			return m, tea.Quit
		case "tab", "shift+tab", "left", "right", "h", "l":
			m.acceptFocus = !m.acceptFocus
		case "up", "k":
			m.offset = max(0, m.offset-1)
		case "down", "j":
			m.offset = min(m.offset+1, m.maxOffset())
		case "pgdown", "space":
			m.offset = min(m.offset+max(1, m.viewH()-1), m.maxOffset())
		case "pgup":
			m.offset = max(0, m.offset-max(1, m.viewH()-1))
		case "enter":
			m.accepted = m.acceptFocus
			return m, tea.Quit
		}
	case tea.MouseWheelMsg:
		switch msg.Mouse().Button {
		case tea.MouseWheelUp:
			m.offset = max(0, m.offset-3)
		case tea.MouseWheelDown:
			m.offset = min(m.offset+3, m.maxOffset())
		}
	case tea.MouseClickMsg:
		m.pressed = -1
		if msg.Mouse().Button == tea.MouseLeft {
			m.pressed = m.buttonAt(msg.Mouse().X, msg.Mouse().Y)
		}
	case tea.MouseReleaseMsg:
		pressed := m.pressed
		m.pressed = -1
		if next := m.buttonAt(msg.Mouse().X, msg.Mouse().Y); pressed >= 0 && next == pressed {
			m.accepted = next == 1
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *review) negative() string {
	if m.back {
		return "Back"
	}
	return "Cancel"
}

// lines is the wrapped, highlighted body.
func (m *review) lines() []string {
	w := max(10, m.width-4)
	var out []string
	for _, l := range strings.Split(cleanMultiline(m.body), "\n") {
		l = strings.ReplaceAll(l, "\t", "    ")
		indent := len(l) - len(strings.TrimLeft(l, " "))
		hang := min(indent+4, w/2)
		first, rest := l, ""
		if ansi.StringWidth(l) > w {
			parts := strings.SplitN(ansi.Wrap(l, w, " ,/"), "\n", 2)
			first, rest = parts[0], strings.TrimLeft(strings.ReplaceAll(parts[1], "\n", ""), " ")
		}
		out = append(out, m.highlight(first, false))
		if rest != "" {
			for _, part := range strings.Split(ansi.Wrap(rest, w-hang, " ,/"), "\n") {
				out = append(out, m.highlight(strings.Repeat(" ", hang)+part, true))
			}
		}
	}
	return out
}

// highlight colors the shapes plan.Render prints: numbered step titles,
// [where] tags, # privileged markers and "! " warnings. Other text is plain.
func (m *review) highlight(l string, continuation bool) string {
	t := m.th
	trim := strings.TrimLeft(l, " ")
	lead := l[:len(l)-len(trim)]
	if continuation {
		return t.Muted.Render(l)
	}
	if n := strings.Index(trim, ". "); n > 0 && n <= 3 && strings.Trim(trim[:n], "0123456789") == "" {
		return lead + t.Accent.Render(trim[:n+1]) + " " + t.Bold.Render(trim[n+2:])
	}
	if strings.HasPrefix(trim, "! ") {
		return lead + t.Warn.Render(trim)
	}
	marker := ""
	if strings.HasPrefix(trim, "# ") {
		marker, trim = t.Warn.Render("#")+" ", trim[2:]
	}
	if strings.HasPrefix(trim, "[") {
		if end := strings.Index(trim, "] "); end > 0 {
			return lead + marker + t.Info.Render(trim[:end+1]) + " " + trim[end+2:]
		}
	}
	return lead + marker + trim
}

func (m *review) viewH() int { return max(1, m.height-5) } // title, box borders, buttons, keys

func (m *review) maxOffset() int { return max(0, len(m.lines())-m.viewH()) }

func (m *review) buttonX() (neg, apply int) {
	return 1, 1 + theme.ButtonWidth(m.negative()) + 2
}

func (m *review) buttonAt(x, y int) int {
	if m.height < 4 || x < 0 || x >= m.width || y != m.height-2 {
		return -1
	}
	neg, apply := m.buttonX()
	switch {
	case x >= neg && x < neg+theme.ButtonWidth(m.negative()):
		return 0
	case x >= apply && x < apply+theme.ButtonWidth("Apply") && apply+theme.ButtonWidth("Apply") <= m.width:
		return 1
	}
	return -1
}

func (m *review) View() tea.View {
	t := m.th
	body := m.lines()
	h := m.viewH()
	start := max(0, min(m.offset, len(body)-h))
	foot := ""
	if len(body) > h {
		foot = fmt.Sprintf("lines %d–%d of %d · ↑↓ PgUp/PgDn scroll", start+1, min(len(body), start+h), len(body))
	}
	lines := []string{" " + t.Accent.Render(clean(m.title))}
	lines = append(lines, t.Box(body[start:], m.width, h+2, theme.BoxOpts{Footer: foot})...)
	neg := t.ButtonText(m.negative(), !m.acceptFocus)
	apply := t.PrimaryText("Apply", m.acceptFocus)
	lines = append(lines, " "+neg+"  "+apply)
	lines = append(lines, " "+strings.Join([]string{t.KeyHint("Tab/←→", "choose"), t.KeyHint("Enter", "confirm"), t.KeyHint("Esc", strings.ToLower(m.negative()))}, t.Muted.Render(" · ")))
	return terminalView(lines, m.width, m.height)
}
func terminalView(lines []string, width, height int) tea.View {
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], max(1, width), "")
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	v := tea.NewView(strings.Join(lines, "\n"))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}
func pad(s string, n int) string {
	if w := ansi.StringWidth(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return ansi.Truncate(s, n, "…")
}
func clean(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
}
func cleanMultiline(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return ' '
		}
		return r
	}, value)
}
