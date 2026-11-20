package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/fleet"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/daviddwlee84/lazylongarm/internal/ui/theme"
)

// demoModel is a populated dashboard for layout tests and visual review.
func demoModel(color bool, w, h int) *Model {
	cfg := &config.Config{
		Hubs:       []config.Hub{{ID: "hub-1", Mode: "public", Address: "hub.example.com:7443", SSHHost: "my-vps", Panel: "127.0.0.1:7480", CAFingerprint: "SHA256:3f9a…c21e", Version: "v0.9.2"}},
		DefaultHub: "hub-1",
		Targets:    []config.Target{{ID: "build-box", Name: "build-box", SSHHost: "build-box", Hub: "hub-1", WorkerName: "build-box"}, {ID: "old-laptop", Name: "old-laptop", SSHHost: "old-laptop", Hub: "hub-1", WorkerName: "old-laptop"}},
	}
	m := New(Options{Config: cfg, Color: color, Mouse: true})
	m.width, m.height = w, h
	now := time.Now()
	ts := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339Nano) }
	node := func(name, state string, online bool, cpu float64, used int64, endpoints ...string) longarm.Node {
		return longarm.Node{Name: name, Role: "worker", State: state, Online: online, OS: "linux", Arch: "amd64", Version: "v0.9.2", Owner: "alice",
			Endpoints: endpoints, Labels: map[string]string{"gpu": "none", "os": "linux"}, LastSeen: ts(3 * time.Second), JobsActive: 1,
			Metrics: &longarm.Metrics{CPUPercent: cpu, MemTotalBytes: 100, MemUsedBytes: longarm.Int64(used)}}
	}
	m.refreshGen = 1
	m.Update(refreshMsg{gen: 1,
		nodes: []longarm.Node{
			node("build-box", "approved", true, 12, 40, "longnet://x"),
			node("gpu-01", "approved", true, 78, 93, "longnet://x", "tcp://10.0.0.5:7701"),
			node("工作站-3", "approved", false, 0, 0),
			node("new-worker", "pending", false, 0, 0),
		},
		jobs: []longarm.FleetJob{
			{Worker: "build-box", WorkerOnline: true, Job: longarm.Job{State: "JOB_STATE_RUNNING", CreatedAt: ts(4 * time.Minute), StartedAt: ts(4 * time.Minute), Spec: longarm.JobSpec{ID: "j-7f3a9c21d0", Name: "nightly-tests", Argv: []string{"make", "test"}, Owner: "alice"}}},
			{Worker: "gpu-01", WorkerOnline: true, Job: longarm.Job{State: "JOB_STATE_FAILED", HasExitCode: true, ExitCode: 2, CreatedAt: ts(9 * time.Minute), StartedAt: ts(9 * time.Minute), EndedAt: ts(7 * time.Minute), Spec: longarm.JobSpec{ID: "j-1b2c3d4e5f", Argv: []string{"python", "train.py", "--epochs", "3"}, Owner: "alice"}}},
			{Worker: "build-box", WorkerOnline: true, Job: longarm.Job{State: "JOB_STATE_SUCCEEDED", HasExitCode: true, CreatedAt: ts(15 * time.Minute), StartedAt: ts(15 * time.Minute), EndedAt: ts(14 * time.Minute), Spec: longarm.JobSpec{ID: "j-9a8b7c6d5e", Argv: []string{"uname", "-a"}, Owner: "alice"}}},
		}})
	var log strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&log, "=== RUN   TestCase%02d\n--- PASS: TestCase%02d (0.%02ds)\n", i, i, i)
	}
	m.logs["j-7f3a9c21d0"] = logState{text: log.String(), fetched: now}
	m.whoami = &longarm.WhoAmI{Name: "laptop", Role: "admin"}
	m.hubChecks = []fleet.HubCheck{
		{ID: "ssh", Status: "ok", Detail: "my-vps reachable"},
		{ID: "service", Status: "ok", Detail: "longarm-hub.service active"},
		{ID: "healthz", Status: "ok", Detail: "https://hub.example.com:7443/healthz 200"},
		{ID: "firewall", Status: "warn", Detail: "udp 7444 not confirmed", Fix: "open udp/7444 in the cloud firewall"},
		{ID: "enrolled", Status: "ok", Detail: "this machine is laptop (admin)"},
	}
	m.doctor = []longarm.DoctorCheck{{Title: "certificate", Status: "ok", Detail: "valid for 29 days"}, {Title: "relay", Status: "fail", Detail: "no relay path", Fix: "check the hub's UDP port"}}
	return m
}

// TestSnapshots writes rendered screens (with ANSI styling) for visual
// review when LAZYLONGARM_SNAPSHOT_DIR is set; render them with a terminal
// screenshot tool (e.g. a VHS tape that cats each file).
func TestSnapshots(t *testing.T) {
	dir := os.Getenv("LAZYLONGARM_SNAPSHOT_DIR")
	if dir == "" {
		t.Skip("set LAZYLONGARM_SNAPSHOT_DIR to write dashboard snapshots")
	}
	for _, size := range [][2]int{{120, 32}, {80, 24}} {
		for _, color := range []bool{true, false} {
			shots := map[string]func(m *Model){
				"fleet":        func(m *Model) {},
				"fleet-detail": func(m *Model) { m.Update(key("j")); m.Update(key("tab")) },
				"jobs":         func(m *Model) { m.Update(key("2")) },
				"hub":          func(m *Model) { m.Update(key("3")) },
				"help":         func(m *Model) { m.Update(key("?")) },
				"kill-confirm": func(m *Model) { m.Update(key("2")); m.Update(key("K")) },
				"jobs-dragcopy": func(m *Model) {
					m.Update(key("2"))
					rows := m.layout().detailRows()
					m.drag = &drag{anchor: rows[len(rows)-6], cur: rows[len(rows)-3], moved: true, done: true}
					m.setStatus("ok", "copied 4 lines to the clipboard")
				},
			}
			for name, setup := range shots {
				m := demoModel(color, size[0], size[1])
				if os.Getenv("LAZYLONGARM_SNAPSHOT_LIGHT") != "" {
					m.th = theme.New(color, false)
				}
				setup(m)
				suffix := ""
				if !color {
					suffix = "-nocolor"
				}
				path := filepath.Join(dir, fmt.Sprintf("dash-%s-%dx%d%s.ansi", name, size[0], size[1], suffix))
				if err := os.WriteFile(path, []byte(m.View().Content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}
