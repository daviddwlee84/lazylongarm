package flows

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daviddwlee84/lazylongarm/internal/companion"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
)

func TestCompanionFerrySetupPlansInstallAndLogin(t *testing.T) {
	f := ubuntu()
	f.Host, f.Has["curl"] = "worker-1", true
	p, err := BuildCompanionFerrySetup(FerrySetupRequest{Host: "worker-1", Name: "worker-1", Login: true, Interactive: true}, f, companion.FerryState{}, FerryDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(p); got != "install-ferry,install-baidupcs,login-baidupcs,verify,save-target" {
		t.Fatalf("steps %s", got)
	}
	js, _ := json.Marshal(p.JSON())
	for _, s := range []string{"install uv into ~/.local/bin", "uv tool install '" + companion.DefaultFerrySpec + "'", "releases/download/" + companion.BaiduPCSVersion + "/BaiduPCS-Go-" + companion.BaiduPCSVersion + "-linux-amd64.zip", "BaiduPCS-Go login", "publishes no checksums"} {
		if !strings.Contains(string(js), s) {
			t.Errorf("plan lacks %q: %s", s, js)
		}
	}
}

func TestCompanionFerrySetupSkipsInstalledAndRecords(t *testing.T) {
	f := ubuntu()
	f.Host = "worker-1"
	cfg := dotfilesCfg(t) // has target worker-1
	state := companion.FerryState{Ferry: "/x/ferry", Version: "0.2.0", UV: "/x/uv", BaiduPCS: "/x/BaiduPCS-Go"}
	deps := FerryDeps{Config: cfg, State: func(context.Context, *plan.Env, string) (companion.FerryState, error) { return state, nil }}
	p, err := BuildCompanionFerrySetup(FerrySetupRequest{Host: "worker-1", Login: true, Interactive: true}, f, state, deps)
	if err != nil {
		t.Fatal(err)
	}
	env := plan.NewEnv()
	var login string
	env.Interactive = func(_ context.Context, r connection.Request) error {
		login = r.Script
		state.BaiduPCSLogin = true
		return nil
	}
	results, err := (&plan.Runner{}).Run(context.Background(), p, env, p.Digest())
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != plan.Skipped || results[1].Status != plan.Skipped || results[2].Status != plan.Done {
		t.Fatalf("results %+v", results)
	}
	if !strings.HasSuffix(login, `exec "$bp" login`) {
		t.Fatalf("login script %q", login)
	}
	tg, _ := cfg.TargetByHost("worker-1")
	if tg.FerryVersion != "0.2.0" || strings.Join(tg.FerryBackends, ",") != "baidupcs" || tg.FerryCheckedAt == "" {
		t.Fatalf("target %+v", tg)
	}
}

func TestCompanionFerrySetupRefusalsAndWarnings(t *testing.T) {
	f := ubuntu()
	if _, err := BuildCompanionFerrySetup(FerrySetupRequest{Host: "h", Login: true}, f, companion.FerryState{UV: "/x"}, FerryDeps{}); err == nil || !strings.Contains(err.Error(), "need a terminal") {
		t.Fatalf("login without terminal: %v", err)
	}
	f.OS = "windows"
	if _, err := BuildCompanionFerrySetup(FerrySetupRequest{Host: "h"}, f, companion.FerryState{UV: "/x"}, FerryDeps{}); err == nil {
		t.Fatal("windows accepted")
	}
	f = ubuntu()
	if _, err := BuildCompanionFerrySetup(FerrySetupRequest{Host: "h", Backends: []string{"dropbox"}}, f, companion.FerryState{UV: "/x"}, FerryDeps{}); err == nil {
		t.Fatal("unknown backend accepted")
	}
	p, err := BuildCompanionFerrySetup(FerrySetupRequest{Host: "h", Backends: []string{companion.BackendRclone, companion.BackendGH}}, f,
		companion.FerryState{Ferry: "/x/ferry", UV: "/x/uv", GH: "/x/gh"}, FerryDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(p); got != "install-ferry,verify,save-target" {
		t.Fatalf("steps %s", got)
	}
	w := strings.Join(p.Warnings, "\n")
	for _, s := range []string{"rclone is not installed on", "gh is not signed in on", "sign in there with gh auth login"} {
		if !strings.Contains(w, s) {
			t.Errorf("warnings lack %q:\n%s", s, w)
		}
	}
}

func ferryPairReq() FerryPairRequest {
	return FerryPairRequest{
		Job:      companion.FerryJob{Name: "run42", Root: "/data/run42", Glob: "*", Dest: "/srv/in", Adapter: "baidupcs", Prefix: "/ferry/run42"},
		Sender:   FerryEnd{Host: "worker-1", Name: "worker-1", Record: "worker-1", OS: "linux", JobPath: "/home/u/.config/ferry/jobs/run42.toml"},
		Receiver: FerryEnd{Host: "worker-2", Name: "worker-2", Record: "worker-2", OS: "darwin", JobPath: "/Users/u/.config/ferry/jobs/run42.toml"},
	}
}

func TestCompanionFerryPairPlan(t *testing.T) {
	ready := companion.FerryState{Ferry: "/x/ferry", BaiduPCS: "/x/b", BaiduPCSLogin: true}
	req := ferryPairReq()
	req.Service = true
	p, err := BuildCompanionFerryPair(req, ready, ready, FerryDeps{})
	if err != nil {
		t.Fatal(err)
	}
	// worker-2 is macOS: only the sender gets a service.
	if got := ids(p); got != "job-worker-1,job-worker-2,doctor-sender,doctor-receiver,plan,service-sender,save-job" {
		t.Fatalf("steps %s", got)
	}
	var b strings.Builder
	p.Render(&b)
	for _, s := range []string{"worker-1:/data/run42 (*)", `dest = "/srv/in"`, "sender worker-1, receiver worker-2", "kept (delete_source = never)", "worker-2 is not Linux", "not the Longarm hub"} {
		if !strings.Contains(b.String(), s) {
			t.Errorf("review lacks %q:\n%s", s, b.String())
		}
	}
	req = ferryPairReq()
	req.Job.DeleteSource = "after_receiver_ack"
	p, _ = BuildCompanionFerryPair(req, ready, ready, FerryDeps{})
	b.Reset()
	p.Render(&b)
	if !strings.Contains(b.String(), "DELETED on the sender") {
		t.Errorf("deletion not called out:\n%s", b.String())
	}
}

func TestCompanionFerryPairRefusals(t *testing.T) {
	ready := companion.FerryState{Ferry: "/x/ferry", BaiduPCS: "/x/b", BaiduPCSLogin: true}
	req := ferryPairReq()
	if _, err := BuildCompanionFerryPair(req, ready, companion.FerryState{Ferry: "/x/ferry", BaiduPCS: "/x/b"}, FerryDeps{}); err == nil || !strings.Contains(err.Error(), "baidupcs is not signed in on worker-2 (run: lazylongarm companion ferry setup worker-2 --backend baidupcs)") {
		t.Fatalf("unsigned receiver: %v", err)
	}
	if _, err := BuildCompanionFerryPair(req, companion.FerryState{}, ready, FerryDeps{}); err == nil || !strings.Contains(err.Error(), "ferry is not installed on worker-1") {
		t.Fatalf("no ferry: %v", err)
	}
	req.Job.Adapter, req.Job.Options = "rclone", map[string]string{"remote": "gdrive:"}
	rc := companion.FerryState{Ferry: "/x/ferry", Rclone: "/x/rclone", RcloneRemotes: []string{"s3:"}}
	if _, err := BuildCompanionFerryPair(req, rc, rc, FerryDeps{}); err == nil || !strings.Contains(err.Error(), `has no remote "gdrive:"`) {
		t.Fatalf("missing rclone remote: %v", err)
	}
	req = ferryPairReq()
	req.Receiver.Host = req.Sender.Host
	if _, err := BuildCompanionFerryPair(req, ready, ready, FerryDeps{}); err == nil {
		t.Fatal("same machine accepted")
	}
}

func TestReadCompanionStateLocally(t *testing.T) {
	s, err := ReadCompanionState(context.Background(), plan.NewEnv(), "")
	if err != nil || s.DotfilesErr != nil || s.FerryErr != nil {
		t.Fatalf("%v %v %v", err, s.DotfilesErr, s.FerryErr)
	}
}
