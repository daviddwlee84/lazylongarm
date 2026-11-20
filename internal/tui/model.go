// Package tui is lazylongarm's dashboard: Fleet, Jobs and Hub tabs over the
// same services as the CLI. Slow work runs in commands; results carry a
// generation so late answers never overwrite newer state. Wizards run as the
// same Cobra commands through Bubble Tea's terminal handoff.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/fleet"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/daviddwlee84/lazylongarm/internal/wizard"
)

// Options configure the dashboard.
type Options struct {
	Ctx        context.Context
	Config     *config.Config
	ReloadCfg  func() (*config.Config, error)
	RunCommand func(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) error
	Refresh    time.Duration
	Color      bool
	OpenURL    func(string) error
}

const (
	tabFleet = iota
	tabJobs
	tabHub
)

var tabNames = []string{"Fleet", "Jobs", "Hub"}

// Model is the dashboard state.
type Model struct {
	opts   Options
	cfg    *config.Config
	client *longarm.Client
	hub    *config.Hub

	width, height int
	tab           int
	focusDetail   bool
	detailScroll  int
	overlay       string // "", "help", "confirm"
	confirm       *confirmation
	pendingG      bool

	rows       []fleet.Row
	jobs       []longarm.FleetJob
	showDone   bool
	selected   [3]string // stable id per tab
	refreshGen uint64
	refreshing bool
	lastOK     time.Time
	refreshErr string
	whoami     *longarm.WhoAmI

	hubChecks  []fleet.HubCheck
	hubGen     uint64
	hubPending bool
	doctor     []longarm.DoctorCheck

	logs         map[string]logState
	logToken     uint64
	execOut      map[string]string
	status       string
	toolBusy     bool
	panel        *connection.Tunnel
	panelPending bool
	quitting     bool
}

type logState struct {
	text    string
	err     string
	fetched time.Time
	pending bool
}

type confirmation struct {
	title, body string
	run         func() tea.Cmd
}

// New builds the model.
func New(opts Options) *Model {
	if opts.Refresh <= 0 {
		opts.Refresh = 5 * time.Second
	}
	if opts.Ctx == nil {
		opts.Ctx = context.Background()
	}
	m := &Model{opts: opts, width: 100, height: 30, logs: map[string]logState{}, execOut: map[string]string{}}
	m.setConfig(opts.Config)
	return m
}

func (m *Model) setConfig(cfg *config.Config) {
	m.cfg = cfg
	m.hub = nil
	if cfg != nil {
		if h, ok := cfg.Hub(""); ok {
			m.hub = h
		}
	}
	dir := ""
	if m.hub != nil {
		dir = m.hub.ConfigDir
	}
	m.client = &longarm.Client{ConfigDir: dir}
}

// Init starts the first refresh and the ticker.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.refresh(), tick(m.opts.Refresh), m.loadWhoAmI())
}

type tickMsg time.Time

func tick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

type refreshMsg struct {
	gen      uint64
	nodes    []longarm.Node
	jobs     []longarm.FleetJob
	nodesErr error
	jobsErr  error
}

func (m *Model) refresh() tea.Cmd {
	if m.refreshing {
		return nil
	}
	m.refreshing = true
	m.refreshGen++
	gen, client, ctx := m.refreshGen, m.client, m.opts.Ctx
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		type res struct {
			nodes []longarm.Node
			err   error
		}
		ch := make(chan res, 1)
		go func() {
			list, err := client.Workers(c)
			ch <- res{list.Nodes, err}
		}()
		jobs, jerr := client.Jobs(c, 200)
		r := <-ch
		return refreshMsg{gen: gen, nodes: r.nodes, jobs: jobs.Jobs, nodesErr: r.err, jobsErr: jerr}
	}
}

type whoamiMsg struct {
	w   *longarm.WhoAmI
	err error
}

func (m *Model) loadWhoAmI() tea.Cmd {
	client, ctx := m.client, m.opts.Ctx
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		w, err := client.WhoAmI(c)
		if err != nil {
			return whoamiMsg{err: err}
		}
		return whoamiMsg{w: &w}
	}
}

type hubStatusMsg struct {
	gen    uint64
	checks []fleet.HubCheck
	doctor []longarm.DoctorCheck
}

func (m *Model) loadHub() tea.Cmd {
	if m.hub == nil || m.hubPending {
		return nil
	}
	m.hubPending = true
	m.hubGen++
	gen, hub, client, ctx := m.hubGen, *m.hub, m.client, m.opts.Ctx
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		checks := fleet.HubStatus(c, &hub)
		d, _ := client.Doctor(c)
		return hubStatusMsg{gen: gen, checks: checks, doctor: d.Checks}
	}
}

type logMsg struct {
	id   string
	text string
	err  error
}
type logDebounceMsg struct {
	token uint64
	id    string
}

func (m *Model) scheduleLog() tea.Cmd {
	j, ok := m.selectedJob()
	if !ok {
		return nil
	}
	m.logToken++
	token, id := m.logToken, j.Job.Spec.ID
	return tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg { return logDebounceMsg{token: token, id: id} })
}

func (m *Model) fetchLog(j longarm.FleetJob) tea.Cmd {
	id := j.Job.Spec.ID
	st := m.logs[id]
	st.pending = true
	m.logs[id] = st
	client, ctx := m.client, m.opts.Ctx
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		out, err := client.Logs(c, j.Worker, id, 16384)
		return logMsg{id: id, text: string(out), err: err}
	}
}

type execMsg struct {
	worker string
	out    string
	err    error
}

func (m *Model) tryWorker(name string) tea.Cmd {
	m.execOut[name] = "running `uname -a`…"
	client, ctx := m.client, m.opts.Ctx
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		out, err := client.Exec(c, name, "uname", "-a")
		return execMsg{worker: name, out: strings.TrimSpace(string(out)), err: err}
	}
}

type killMsg struct {
	id  string
	err error
}

func (m *Model) kill(j longarm.FleetJob) tea.Cmd {
	client, ctx := m.client, m.opts.Ctx
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		err := client.Kill(c, j.Worker, j.Job.Spec.ID, j.Job.CreatedAt)
		return killMsg{id: j.Job.Spec.ID, err: err}
	}
}

type panelMsg struct {
	t   *connection.Tunnel
	err error
}
type panelClosedMsg struct{ t *connection.Tunnel }

// togglePanel opens (or closes) a dashboard-owned tunnel to the hub's
// loopback panel; the dashboard stays usable and closes it on exit.
func (m *Model) togglePanel() tea.Cmd {
	if m.panel != nil {
		_ = m.panel.Close()
		m.panel = nil
		m.status = "panel tunnel closed"
		return nil
	}
	if m.hub == nil || m.hub.SSHHost == "" || m.panelPending {
		return nil
	}
	m.panelPending = true
	m.status = "opening the panel tunnel…"
	host, remote, ctx := m.hub.SSHHost, m.hub.Panel, m.opts.Ctx
	if remote == "" {
		remote = "127.0.0.1:7480"
	}
	return func() tea.Msg {
		t, err := connection.OpenTunnel(ctx, host, 0, remote)
		return panelMsg{t: t, err: err}
	}
}

func waitPanel(t *connection.Tunnel) tea.Cmd {
	return func() tea.Msg { <-t.Done(); return panelClosedMsg{t: t} }
}

// PanelURL is the local URL of the open panel tunnel ("" when closed).
func (m *Model) PanelURL() string {
	if m.panel == nil {
		return ""
	}
	return "http://" + m.panel.Address + "/"
}

// Close releases resources the dashboard owns (the panel tunnel).
func (m *Model) Close() {
	if m.panel != nil {
		_ = m.panel.Close()
		m.panel = nil
	}
}

type toolDoneMsg struct {
	label string
	err   error
}

// runTool hands the terminal to the same Cobra command used standalone.
func (m *Model) runTool(label string, args ...string) tea.Cmd {
	if m.toolBusy || m.opts.RunCommand == nil {
		return nil
	}
	m.toolBusy = true
	m.status = label + "…"
	task := &terminalTask{ctx: m.opts.Ctx, args: args, run: m.opts.RunCommand}
	return tea.Exec(task, func(err error) tea.Msg { return toolDoneMsg{label: label, err: err} })
}

// runProcess hands the terminal to a longarm subcommand (ssh, logs -f).
func (m *Model) runProcess(label string, args ...string) tea.Cmd {
	if m.toolBusy {
		return nil
	}
	cmd, err := m.client.Command(m.opts.Ctx, args...)
	if err != nil {
		m.status = err.Error()
		return nil
	}
	m.toolBusy = true
	m.status = label + "…"
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return toolDoneMsg{label: label, err: err} })
}

type terminalTask struct {
	ctx         context.Context
	args        []string
	run         func(context.Context, []string, io.Reader, io.Writer, io.Writer) error
	in          io.Reader
	out, errOut io.Writer
}

func (t *terminalTask) SetStdin(in io.Reader)   { t.in = in }
func (t *terminalTask) SetStdout(out io.Writer) { t.out = out }
func (t *terminalTask) SetStderr(out io.Writer) { t.errOut = out }
func (t *terminalTask) Run() error {
	err := t.run(t.ctx, t.args, t.in, t.out, t.errOut)
	if errors.Is(err, wizard.ErrCanceled) || errors.Is(err, context.Canceled) {
		return err
	}
	label := "Finished"
	if err != nil {
		label = "Did not complete"
		fmt.Fprintln(t.errOut, "lazylongarm:", err)
	}
	_ = wizard.Pause(t.ctx, label, t.in, t.errOut)
	return err
}

var _ tea.ExecCommand = (*terminalTask)(nil)
var _ = exec.Command

// Update handles messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(20, msg.Width), max(8, msg.Height)
		return m, nil
	case tickMsg:
		cmds := []tea.Cmd{tick(m.opts.Refresh)}
		if !m.toolBusy {
			cmds = append(cmds, m.refresh())
			if j, ok := m.selectedJob(); ok && m.tab == tabJobs && j.Job.Active() {
				cmds = append(cmds, m.fetchLog(j))
			}
		}
		return m, tea.Batch(cmds...)
	case refreshMsg:
		if msg.gen != m.refreshGen {
			return m, nil
		}
		m.refreshing = false
		var errs []string
		if msg.nodesErr == nil {
			m.rows = fleet.Merge(m.cfg, msg.nodes)
		} else {
			errs = append(errs, "workers: "+shortErr(msg.nodesErr))
			if len(m.rows) == 0 {
				m.rows = fleet.Merge(m.cfg, nil)
			}
		}
		if msg.jobsErr == nil {
			m.jobs = msg.jobs
			sortJobs(m.jobs)
		} else {
			errs = append(errs, "jobs: "+shortErr(msg.jobsErr))
		}
		if len(errs) == 0 {
			m.lastOK, m.refreshErr = time.Now(), ""
		} else {
			m.refreshErr = strings.Join(errs, "; ")
		}
		m.clampSelection()
		if m.tab == tabJobs {
			if j, ok := m.selectedJob(); ok {
				if _, have := m.logs[j.Job.Spec.ID]; !have {
					return m, m.fetchLog(j)
				}
			}
		}
		return m, nil
	case whoamiMsg:
		m.whoami = msg.w
		return m, nil
	case hubStatusMsg:
		if msg.gen == m.hubGen {
			m.hubPending = false
			m.hubChecks, m.doctor = msg.checks, msg.doctor
		}
		return m, nil
	case logDebounceMsg:
		if msg.token != m.logToken {
			return m, nil
		}
		if j, ok := m.selectedJob(); ok && j.Job.Spec.ID == msg.id {
			return m, m.fetchLog(j)
		}
		return m, nil
	case logMsg:
		st := logState{text: msg.text, fetched: time.Now()}
		if msg.err != nil {
			st.err = shortErr(msg.err)
			st.text = m.logs[msg.id].text
		}
		m.logs[msg.id] = st
		return m, nil
	case execMsg:
		if msg.err != nil {
			m.execOut[msg.worker] = "✗ " + shortErr(msg.err)
		} else {
			m.execOut[msg.worker] = msg.out
		}
		return m, nil
	case killMsg:
		if msg.err != nil {
			m.status = "kill " + short(msg.id) + ": " + shortErr(msg.err) + " — refresh before retrying"
		} else {
			m.status = "killed " + short(msg.id)
		}
		return m, m.refresh()
	case panelMsg:
		m.panelPending = false
		if msg.err != nil {
			m.status = "panel tunnel: " + shortErr(msg.err)
			return m, nil
		}
		m.panel = msg.t
		m.status = "panel: " + m.PanelURL() + " (log in with a panel account + TOTP; p closes, U creates a login)"
		if m.opts.OpenURL != nil {
			_ = m.opts.OpenURL(m.PanelURL())
		}
		return m, waitPanel(msg.t)
	case panelClosedMsg:
		if m.panel == msg.t {
			m.panel = nil
			m.status = "panel tunnel ended"
		}
		return m, nil
	case toolDoneMsg:
		m.toolBusy = false
		switch {
		case msg.err == nil:
			m.status = msg.label + ": done"
		case errors.Is(msg.err, wizard.ErrCanceled), errors.Is(msg.err, context.Canceled):
			m.status = msg.label + ": canceled"
		default:
			m.status = msg.label + ": " + shortErr(msg.err)
		}
		if m.opts.ReloadCfg != nil {
			if cfg, err := m.opts.ReloadCfg(); err == nil {
				m.setConfig(cfg)
			}
		}
		m.refreshing = false
		cmds := []tea.Cmd{m.refresh(), m.loadWhoAmI()}
		if m.tab == tabHub {
			m.hubPending = false
			cmds = append(cmds, m.loadHub())
		}
		return m, tea.Batch(cmds...)
	case tea.KeyPressMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m *Model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k == "ctrl+c" {
		m.quitting = true
		return m, tea.Quit
	}
	switch m.overlay {
	case "help":
		if k == "esc" || k == "?" || k == "q" || k == "enter" {
			m.overlay = ""
		}
		return m, nil
	case "confirm":
		switch k {
		case "y", "Y":
			c := m.confirm
			m.overlay, m.confirm = "", nil
			if c != nil {
				return m, c.run()
			}
		case "n", "N", "esc", "q":
			m.overlay, m.confirm = "", nil
			m.status = "canceled"
		}
		return m, nil
	}
	if m.pendingG {
		m.pendingG = false
		if k == "g" {
			m.moveTo(0)
			return m, m.afterMove()
		}
	}
	switch k {
	case "q":
		m.quitting = true
		return m, tea.Quit
	case "?":
		m.overlay = "help"
		return m, nil
	case "1", "2", "3":
		return m, m.switchTab(int(k[0] - '1'))
	case "left", "h", "shift+tab":
		if k == "shift+tab" && m.focusDetail {
			m.focusDetail = false
			return m, nil
		}
		return m, m.switchTab((m.tab + len(tabNames) - 1) % len(tabNames))
	case "right", "l":
		return m, m.switchTab((m.tab + 1) % len(tabNames))
	case "tab":
		m.focusDetail = !m.focusDetail
		return m, nil
	case "up", "k":
		if m.focusDetail {
			m.detailScroll = max(0, m.detailScroll-1)
			return m, nil
		}
		m.move(-1)
		return m, m.afterMove()
	case "down", "j":
		if m.focusDetail {
			m.detailScroll++
			return m, nil
		}
		m.move(1)
		return m, m.afterMove()
	case "g":
		m.pendingG = true
		return m, nil
	case "G", "end":
		m.moveTo(1 << 30)
		return m, m.afterMove()
	case "home":
		m.moveTo(0)
		return m, m.afterMove()
	case "r":
		m.refreshing = false
		cmds := []tea.Cmd{m.refresh()}
		if m.tab == tabHub {
			m.hubPending = false
			cmds = append(cmds, m.loadHub())
		}
		if j, ok := m.selectedJob(); ok && m.tab == tabJobs {
			cmds = append(cmds, m.fetchLog(j))
		}
		m.status = "refreshing"
		return m, tea.Batch(cmds...)
	}
	for _, a := range m.actions() {
		if a.enabled && containsKey(a.keys, k) {
			return m, a.run()
		}
	}
	return m, nil
}

func (m *Model) switchTab(t int) tea.Cmd {
	m.tab = t
	m.focusDetail = false
	m.detailScroll = 0
	m.clampSelection()
	switch t {
	case tabHub:
		if m.hubChecks == nil {
			return m.loadHub()
		}
	case tabJobs:
		return m.scheduleLog()
	}
	return nil
}

func (m *Model) afterMove() tea.Cmd {
	m.detailScroll = 0
	if m.tab == tabJobs {
		return m.scheduleLog()
	}
	return nil
}

// ---- selection by identity ----

func (m *Model) ids() []string {
	switch m.tab {
	case tabFleet:
		out := make([]string, len(m.rows))
		for i, r := range m.rows {
			out[i] = r.Name
		}
		return out
	case tabJobs:
		var out []string
		for _, j := range m.visibleJobs() {
			out = append(out, j.Job.Spec.ID+"@"+j.Job.CreatedAt)
		}
		return out
	}
	return nil
}

func (m *Model) index() int {
	ids := m.ids()
	for i, id := range ids {
		if id == m.selected[m.tab] {
			return i
		}
	}
	return 0
}

func (m *Model) move(delta int) { m.moveTo(m.index() + delta) }
func (m *Model) moveTo(i int) {
	ids := m.ids()
	if len(ids) == 0 {
		return
	}
	i = max(0, min(i, len(ids)-1))
	m.selected[m.tab] = ids[i]
}

func (m *Model) clampSelection() {
	for _, t := range []int{tabFleet, tabJobs} {
		saved := m.tab
		m.tab = t
		ids := m.ids()
		found := false
		for _, id := range ids {
			if id == m.selected[t] {
				found = true
			}
		}
		if !found && len(ids) > 0 {
			m.selected[t] = ids[0]
		}
		m.tab = saved
	}
}

func (m *Model) selectedRow() (fleet.Row, bool) {
	for _, r := range m.rows {
		if r.Name == m.selected[tabFleet] {
			return r, true
		}
	}
	return fleet.Row{}, false
}

func (m *Model) visibleJobs() []longarm.FleetJob {
	if m.showDone {
		return m.jobs
	}
	var out []longarm.FleetJob
	cutoff := time.Now().Add(-30 * time.Minute)
	for _, j := range m.jobs {
		if j.Job.Active() {
			out = append(out, j)
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, j.Job.EndedAt); err == nil && t.After(cutoff) {
			out = append(out, j)
		}
	}
	return out
}

func (m *Model) selectedJob() (longarm.FleetJob, bool) {
	for _, j := range m.visibleJobs() {
		if j.Job.Spec.ID+"@"+j.Job.CreatedAt == m.selected[tabJobs] {
			return j, true
		}
	}
	return longarm.FleetJob{}, false
}

func sortJobs(jobs []longarm.FleetJob) {
	sort.SliceStable(jobs, func(i, j int) bool {
		ai, aj := jobs[i].Job.Active(), jobs[j].Job.Active()
		if ai != aj {
			return ai
		}
		return jobs[i].Job.CreatedAt > jobs[j].Job.CreatedAt
	})
}

// ---- actions (one registry for keys, footer and help) ----

type action struct {
	id, label string
	keys      []string
	enabled   bool
	run       func() tea.Cmd
}

func (m *Model) actions() []action {
	var out []action
	hasHub := m.hub != nil
	switch m.tab {
	case tabFleet:
		row, ok := m.selectedRow()
		out = append(out,
			action{"add", "add worker", []string{"a"}, hasHub && !m.toolBusy, func() tea.Cmd { return m.runTool("Add worker", "worker", "add") }},
			action{"try", "try uname", []string{"x", "enter"}, ok && row.Online, func() tea.Cmd { return m.tryWorker(row.Name) }},
			action{"ssh", "longarm ssh", []string{"s"}, ok && row.InHub && !m.toolBusy, func() tea.Cmd { return m.runProcess("longarm ssh "+row.Name, "ssh", row.Name) }},
		)
	case tabJobs:
		j, ok := m.selectedJob()
		out = append(out,
			action{"follow", "follow log", []string{"f"}, ok && !m.toolBusy, func() tea.Cmd {
				return m.runProcess("longarm logs -f", "logs", j.Worker, j.Job.Spec.ID, "-f", "--from", "-65536")
			}},
			action{"kill", "kill", []string{"K"}, ok && j.Job.Active(), func() tea.Cmd {
				m.overlay = "confirm"
				m.confirm = &confirmation{
					title: "Kill job " + short(j.Job.Spec.ID),
					body:  fmt.Sprintf("Kill %s on %s?\n\nThe job's process tree is terminated and the job ends as killed (exit 137).\nOnly this generation (created %s) is affected.", jobLabel(j.Job), j.Worker, j.Job.CreatedAt),
					run:   func() tea.Cmd { m.status = "killing " + short(j.Job.Spec.ID); return m.kill(j) },
				}
				return nil
			}},
			action{"done", map[bool]string{true: "hide finished", false: "show finished"}[m.showDone], []string{"t"}, true, func() tea.Cmd {
				m.showDone = !m.showDone
				m.clampSelection()
				return m.scheduleLog()
			}},
		)
	case tabHub:
		out = append(out,
			action{"setup", map[bool]string{true: "rerun hub setup", false: "hub setup"}[hasHub], []string{"S"}, !m.toolBusy, func() tea.Cmd { return m.runTool("Hub setup", "hub", "setup") }},
			action{"panel", map[bool]string{true: "close panel", false: "web panel"}[m.panel != nil], []string{"p"}, hasHub && m.hub.SSHHost != "" && !m.panelPending, func() tea.Cmd { return m.togglePanel() }},
			action{"panel-user", "panel logins", []string{"U"}, hasHub && m.hub.SSHHost != "" && !m.toolBusy, func() tea.Cmd { return m.runTool("Panel login", "hub", "panel-user") }},
		)
	}
	return out
}

func jobLabel(j longarm.Job) string {
	cmd := strings.Join(j.Spec.Argv, " ")
	if j.Spec.Name == "" {
		return "`" + cmd + "`"
	}
	return fmt.Sprintf("%q (%s)", j.Spec.Name, cmd)
}

func containsKey(keys []string, k string) bool {
	for _, x := range keys {
		if x == k {
			return true
		}
	}
	return false
}

func short(id string) string {
	if len(id) > 10 {
		return id[:10]
	}
	return id
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
