package flows

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/hostprobe"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/secret"
)

func ubuntu() *hostprobe.Facts {
	f, _ := hostprobe.Parse(`os=Linux
arch=x86_64
distro_id=ubuntu
distro_pretty=Ubuntu 22.04.2 LTS
user=david
home=/home/david
sudo=nopasswd
has_systemctl=yes
has_apt-get=yes
has_curl=yes
has_gpg=yes
mem_total_kb=1893040
mem_avail_kb=1109092
cpus=1
listen=tcp 0.0.0.0:443
container=frps
probe_done=yes
`)
	return f
}

func ids(p *plan.Plan) string {
	var out []string
	for _, s := range p.Steps {
		out = append(out, s.ID)
	}
	return strings.Join(out, ",")
}

func TestHubPlanSteps(t *testing.T) {
	p, err := BuildHubSetup(HubRequest{Host: "my-vps", Address: "hub.example.com", EnrollLocal: true}, ubuntu(), HubDeps{Locals: &LocalDeps{}})
	if err != nil {
		t.Fatal(err)
	}
	want := "apt-repo,install-hub,data-dir,hub-config,bootstrap,service,fingerprint,ports,reachability,save-hub,local-longarm,enroll-client"
	if got := ids(p); got != want {
		t.Fatalf("steps\n got %s\nwant %s", got, want)
	}
	var b strings.Builder
	p.Render(&b)
	for _, s := range []string{"public_addr = \"hub.example.com:7443\"", "runuser -u longarm-hub", "proxy \"frps\""} {
		if !strings.Contains(b.String(), s) {
			t.Errorf("review lacks %q", s)
		}
	}
}

func TestDefaultHubName(t *testing.T) {
	cases := map[string]string{"admin@hub-1.japaneast.cloudapp.azure.com": "hub-1", "my-vps": "my-vps", "root@10.0.0.5": "10-0-0-5", "ts_nas": "ts-nas"}
	for in, want := range cases {
		if got := DefaultHubName(in); got != want {
			t.Errorf("DefaultHubName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHubValidationCatchesBusyPortsAndUnsupportedHosts(t *testing.T) {
	f := ubuntu()
	f.Listening = append(f.Listening, "udp 0.0.0.0:7444")
	if err := ValidateHub(HubDefaults(HubRequest{Host: "h", Address: "a.example"}, f), f); err == nil || !strings.Contains(err.Error(), "udp/7444") {
		t.Fatalf("busy port not detected: %v", err)
	}
	mac := &hostprobe.Facts{OS: "darwin", Arch: "arm64", Distro: "macOS", Has: map[string]bool{}}
	if err := ValidateHub(HubDefaults(HubRequest{Host: "m", Address: "x"}, mac), mac); err == nil || !strings.Contains(err.Error(), "Debian/Ubuntu") {
		t.Fatalf("macOS hub accepted: %v", err)
	}
	if err := ValidateHub(HubDefaults(HubRequest{Host: "h", Address: "a.example:7443"}, f), f); err == nil {
		t.Fatal("address with port accepted")
	}
}

type fakeClient struct {
	approved []string
	nodes    []longarm.Node
	code     secret.Secret
	execed   []string
}

func (f *fakeClient) Version(context.Context) (longarm.VersionInfo, error) {
	return longarm.VersionInfo{Version: "v0.1.42"}, nil
}
func (f *fakeClient) WhoAmI(context.Context) (longarm.WhoAmI, error) {
	return longarm.WhoAmI{Role: "admin"}, nil
}
func (f *fakeClient) Workers(context.Context) (longarm.WorkerList, error) {
	return longarm.WorkerList{Nodes: f.nodes}, nil
}
func (f *fakeClient) WorkerAdd(context.Context, string, time.Duration, []string) (secret.Secret, error) {
	return f.code, nil
}
func (f *fakeClient) AdminJoinCode(context.Context, string, time.Duration, []string) (secret.Secret, error) {
	return f.code, nil
}
func (f *fakeClient) AdminApprove(_ context.Context, name string) error {
	f.approved = append(f.approved, name)
	return nil
}
func (f *fakeClient) Join(context.Context, string, secret.Secret, string) ([]byte, error) {
	return nil, nil
}
func (f *fakeClient) DaemonStart(context.Context) error { return nil }
func (f *fakeClient) Exec(_ context.Context, w string, argv ...string) ([]byte, error) {
	f.execed = append(f.execed, w)
	return []byte("Linux box"), nil
}

func TestWorkerPlanAndOnlineWait(t *testing.T) {
	hub := &config.Hub{ID: "hub-1", Address: "hub.example.com:7443", CAFingerprint: "sha256:aa", Mode: config.ModePublic}
	client := &fakeClient{code: secret.New("C0DE-1"), nodes: []longarm.Node{{Name: "worker-1", Role: "worker", State: "approved", Online: true, Endpoints: []string{"longnet://hub.example.com:7443?name=worker-1"}}}}
	p, err := BuildWorkerAdd(WorkerRequest{Host: "worker-1", Hub: hub, Version: "v0.1.42"}, ubuntu(), WorkerDeps{Client: client, PollInterval: time.Millisecond, WaitTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(p); got != "binaries,enroll,service,online,try-it,save-target" {
		t.Fatalf("steps %s", got)
	}
	var b strings.Builder
	p.Render(&b)
	if strings.Contains(b.String(), "C0DE-1") || !strings.Contains(b.String(), "<code from stdin>") {
		t.Fatalf("review must not contain the code:\n%s", b.String())
	}
	env := plan.NewEnv()
	for _, s := range p.Steps {
		if s.ID == "online" || s.ID == "try-it" {
			if err := s.Apply(context.Background(), env); err != nil {
				t.Fatalf("%s: %v", s.ID, err)
			}
		}
	}
	if env.Get("worker_path") != "relay" || len(client.execed) != 1 {
		t.Fatalf("path %q exec %v", env.Get("worker_path"), client.execed)
	}
}

func TestWorkerValidation(t *testing.T) {
	f := ubuntu()
	if err := ValidateWorker(WorkerDefaults(WorkerRequest{Host: "h"}, f), f); err == nil || !strings.Contains(err.Error(), "no hub") {
		t.Fatalf("missing hub accepted: %v", err)
	}
	hub := &config.Hub{ID: "h", Address: "a:7443", CAFingerprint: "x"}
	if err := ValidateWorker(WorkerRequest{Name: "bad name", Hub: hub, Service: ServiceSystem}, f); err == nil {
		t.Fatal("bad name accepted")
	}
	if err := ValidateWorker(WorkerRequest{Name: "ok", Hub: hub, Service: ServiceSystem, DirectTailnet: true}, f); err == nil {
		t.Fatal("direct tailnet without a tailnet IP accepted")
	}
}

func TestAdminMintApprovesPendingWorker(t *testing.T) {
	hub := &config.Hub{ID: "h", Address: "hub.example.com:7443", CAFingerprint: "sha256:aa", Mode: config.ModePublic}
	client := &fakeClient{code: secret.New("ABC123"), nodes: []longarm.Node{{Name: "w1", Role: "worker", State: "pending"}}}
	p, err := BuildWorkerAdd(WorkerRequest{Host: "w1", Hub: hub, Version: "v0.1.42", Mint: MintAdmin}, ubuntu(), WorkerDeps{Client: client, PollInterval: time.Millisecond, WaitTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	p.Render(&b)
	if !strings.Contains(b.String(), "longarm admin join-code --role worker --name w1") || !strings.Contains(b.String(), "longarm admin approve w1") {
		t.Fatalf("review lacks the admin path:\n%s", b.String())
	}
	for _, s := range p.Steps {
		if s.ID == "online" {
			_ = s.Apply(context.Background(), plan.NewEnv()) // times out: the fake never comes online
		}
	}
	if len(client.approved) != 1 || client.approved[0] != "w1" {
		t.Fatalf("pending worker not approved exactly once: %v", client.approved)
	}
}
