package fleet

import (
	"strings"
	"testing"

	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/hubhost"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
)

func TestHealthOKAcceptsPrettyJSON(t *testing.T) {
	body := "{\n  \"ok\": true,\n  \"online\": 0,\n  \"version\": \"v0.1.42\"\n}"
	if !hubhost.HealthOK(body) {
		t.Fatal("pretty-printed healthz rejected")
	}
	if hubhost.HealthOK(`{"ok": false}`) || hubhost.HealthOK("unhealthy") {
		t.Fatal("unhealthy accepted")
	}
	if got := hubhost.HealthSummary(body); !strings.Contains(got, "ok=true") || !strings.Contains(got, "version=v0.1.42") {
		t.Fatalf("summary %q", got)
	}
}

func TestMergeKeepsManagedTargetsMissingFromHub(t *testing.T) {
	cfg := &config.Config{Targets: []config.Target{{ID: "box", WorkerName: "box", SSHHost: "box", OS: "linux", Arch: "amd64"}, {ID: "w1", WorkerName: "w1", SSHHost: "w1"}}}
	rows := Merge(cfg, []longarm.Node{{Name: "w1", Role: "worker", State: "approved", Online: true}, {Name: "mac", Role: "admin"}})
	if len(rows) != 2 || rows[0].Name != "w1" || !rows[0].Managed || rows[0].SSHHost != "w1" || rows[1].State != "not in hub" {
		t.Fatalf("%+v", rows)
	}
}

func TestRowLabelTextSkipsPlatform(t *testing.T) {
	r := Row{Labels: map[string]string{"os": "linux", "arch": "amd64", "pool": "home", "gpu": "3090"}}
	if got := r.LabelText(); got != "gpu=3090 pool=home" {
		t.Fatalf("%q", got)
	}
	if (Row{}).LabelText() != "" {
		t.Fatal("empty labels")
	}
}

func TestMergeShowsTheLiveNodeOfAReenrolledMachine(t *testing.T) {
	for _, nodes := range [][]longarm.Node{
		{{ID: "new", Name: "w1", State: "approved", Online: true, Owner: "alice"}, {ID: "old", Name: "w1", State: "revoked"}},
		{{ID: "old", Name: "w1", State: "revoked"}, {ID: "new", Name: "w1", State: "approved", Online: true, Owner: "alice"}},
	} {
		rows := Merge(nil, nodes)
		if len(rows) != 1 || rows[0].State != "online" || rows[0].Owner != "alice" {
			t.Fatalf("rows %+v", rows)
		}
	}
	if rows := Merge(nil, []longarm.Node{{Name: "gone", State: "revoked"}}); len(rows) != 1 || rows[0].State != "revoked" {
		t.Fatalf("a revoked-only node: %+v", rows)
	}
}
