package flows

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
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
	want := "apt-repo,install-hub,data-dir,hub-config,bootstrap,service,restart-hub,fingerprint,ports,reachability,save-hub,local-longarm,enroll-client"
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
	approved  []string
	revoked   []string
	nodes     []longarm.Node
	code      secret.Secret
	execed    []string
	joinCodes []longarm.JoinCodeRequest
	who       longarm.WhoAmI
	users     longarm.UserList
	userAdds  []longarm.UserAddRequest
	added     longarm.UserAdded
	joins     []string // "join" | "join --force"
	calls     []string // order of side effects
}

func (f *fakeClient) Version(context.Context) (longarm.VersionInfo, error) {
	return longarm.VersionInfo{Version: "v0.1.42"}, nil
}
func (f *fakeClient) WhoAmI(context.Context) (longarm.WhoAmI, error) {
	if f.who.Role == "" {
		return longarm.WhoAmI{Role: "admin"}, nil
	}
	return f.who, nil
}
func (f *fakeClient) AdminRevoke(_ context.Context, name string) error {
	f.revoked = append(f.revoked, name)
	f.calls = append(f.calls, "revoke "+name)
	return nil
}
func (f *fakeClient) AdminUsers(context.Context) (longarm.UserList, error) { return f.users, nil }
func (f *fakeClient) AdminUserAdd(_ context.Context, req longarm.UserAddRequest) (longarm.UserAdded, error) {
	f.userAdds = append(f.userAdds, req)
	f.calls = append(f.calls, "user add "+req.Name)
	return f.added, nil
}
func (f *fakeClient) AdminUserCode(_ context.Context, req longarm.UserAddRequest) (longarm.UserAdded, error) {
	f.userAdds = append(f.userAdds, req)
	f.calls = append(f.calls, "user code "+req.Name)
	return f.added, nil
}
func (f *fakeClient) JoinForce(context.Context, string, secret.Secret, string) ([]byte, error) {
	f.joins = append(f.joins, "join --force")
	f.calls = append(f.calls, "join --force")
	return nil, nil
}
func (f *fakeClient) DaemonStop(context.Context) error {
	f.calls = append(f.calls, "daemon stop")
	return nil
}
func (f *fakeClient) Workers(context.Context) (longarm.WorkerList, error) {
	return longarm.WorkerList{Nodes: f.nodes}, nil
}
func (f *fakeClient) WorkerAdd(context.Context, string, time.Duration, []string) (secret.Secret, error) {
	return f.code, nil
}
func (f *fakeClient) AdminJoinCode(_ context.Context, req longarm.JoinCodeRequest) (secret.Secret, error) {
	f.joinCodes = append(f.joinCodes, req)
	f.calls = append(f.calls, "join-code "+req.Name)
	return f.code, nil
}
func (f *fakeClient) AdminApprove(_ context.Context, name string) error {
	f.approved = append(f.approved, name)
	return nil
}
func (f *fakeClient) Join(context.Context, string, secret.Secret, string) ([]byte, error) {
	f.joins = append(f.joins, "join")
	return nil, nil
}
func (f *fakeClient) DaemonStart(context.Context) error {
	f.calls = append(f.calls, "daemon start")
	return nil
}
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

// Two overlapping `worker add` runs each hold the config loaded at startup;
// the one that finishes second must still record its target, and so must a
// retry of the step.
func TestOverlappingWorkerAddsBothRecordTargets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	hub := config.Hub{ID: "hub-1", Address: "hub.example.com:7443", CAFingerprint: "sha256:aa", Mode: config.ModePublic}
	seed, _ := config.Load(path, false)
	if err := seed.Update(func(c *config.Config) error { c.UpsertHub(hub); return nil }); err != nil {
		t.Fatal(err)
	}
	saveStep := func(cfg *config.Config, host string) plan.Step {
		p, err := BuildWorkerAdd(WorkerRequest{Host: host, Hub: &hub, Version: "v0.1.42"}, ubuntu(), WorkerDeps{Config: cfg, Client: &fakeClient{}})
		if err != nil {
			t.Fatal(err)
		}
		return p.Steps[len(p.Steps)-1]
	}
	a, _ := config.Load(path, true)
	b, _ := config.Load(path, true)
	stepA, stepB := saveStep(a, "worker-1"), saveStep(b, "worker-2")
	for _, run := range []struct {
		name string
		step plan.Step
	}{{"worker-1", stepA}, {"worker-2", stepB}, {"worker-2 retry", stepB}} {
		if run.step.ID != "save-target" {
			t.Fatalf("last step %s", run.step.ID)
		}
		if err := run.step.Apply(context.Background(), plan.NewEnv()); err != nil {
			t.Fatalf("%s: %v", run.name, err)
		}
	}
	loaded, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Targets) != 2 || loaded.Targets[0].ID != "worker-1" || loaded.Targets[1].ID != "worker-2" {
		t.Fatalf("targets %+v", loaded.Targets)
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

func TestPeerPlanSteps(t *testing.T) {
	hub := &config.Hub{ID: "hub-1", Address: "hub.example.com:7443", CAFingerprint: "sha256:aa", Mode: config.ModePublic}
	client := &fakeClient{code: secret.New("C0DE-2")}
	f := ubuntu()
	f.Groups = []string{"david", "docker"}
	p, err := BuildPeerAdd(PeerRequest{WorkerRequest: WorkerRequest{Host: "worker-1", Hub: hub, Version: "v0.1.42"}}, f, WorkerDeps{Client: client})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(p); got != "binaries,enroll,service,online,try-it,enroll-client,try-peer,save-target" {
		t.Fatalf("steps %s", got)
	}
	if p.Kind != "peer-add" {
		t.Fatalf("kind %q", p.Kind)
	}
	var b strings.Builder
	p.Render(&b)
	review := b.String()
	for _, s := range []string{"--role client --name worker-1-client", "join hub.example.com:7443 <code from stdin>", "longarm admin revoke worker-1-client", "group docker, passwordless sudo"} {
		if !strings.Contains(review, s) {
			t.Errorf("review lacks %q", s)
		}
	}
	if strings.Contains(review, "C0DE-2") {
		t.Fatal("review shows the code")
	}
	if len(client.joinCodes) != 0 {
		t.Fatal("building the plan minted a code")
	}
}

func TestPeerValidation(t *testing.T) {
	hub := &config.Hub{ID: "hub-1", Address: "hub.example.com:7443", CAFingerprint: "sha256:aa"}
	base := WorkerRequest{Host: "worker-1", Hub: hub}
	cases := []struct {
		req  PeerRequest
		want string
	}{
		{PeerRequest{WorkerRequest: base, ClientName: "worker-1"}, "must differ from the worker name"},
		{PeerRequest{WorkerRequest: WorkerRequest{Host: "worker-1", Hub: hub, Mint: MintOwner}}, "needs an admin certificate"},
		{PeerRequest{WorkerRequest: base, Tenancy: true}, "--client-user"},
		{PeerRequest{WorkerRequest: base, ClientName: "bad name"}, "client name"},
		{PeerRequest{WorkerRequest: base, Tenancy: true, ClientUser: "alice", ClientUserDevices: 1}, "admin panel login named alice"},
		{PeerRequest{WorkerRequest: base, Tenancy: true, ClientUser: "alice", ClientUserDevices: 1, ClientUserPanel: true}, "alice's panel login (a client certificate)"},
	}
	for _, tc := range cases {
		req := PeerDefaults(tc.req, ubuntu())
		if err := ValidatePeer(req, ubuntu()); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: got %v, want %q", tc.req, err, tc.want)
		}
	}
	ok := PeerDefaults(PeerRequest{WorkerRequest: base, Tenancy: true, ClientUser: "alice"}, ubuntu())
	if err := ValidatePeer(ok, ubuntu()); err != nil || ok.ClientName != "worker-1-client" {
		t.Fatalf("valid peer: %v (%q)", err, ok.ClientName)
	}
}

func TestUserDaemonPatternSkipsTheCheckingShell(t *testing.T) {
	re := regexp.MustCompile(userDaemonPattern)
	check := `pgrep -u "$(id -u)" -f '` + userDaemonPattern + `' >/dev/null 2>&1`
	for _, cmdline := range []string{
		"/home/user/.local/bin/longarmd run --data-dir /home/user/.longarm",
		"longarmd run",
	} {
		if !re.MatchString(cmdline) {
			t.Errorf("pattern misses the daemon %q", cmdline)
		}
	}
	for _, cmdline := range []string{
		"sh -c " + check,                   // the check's own shell
		"zsh -c 'sh' '-c' '" + check + "'", // the login shell running it over ssh
		"vim notes about longarmd run",
		"/home/user/.local/bin/longarmd runner",
	} {
		if re.MatchString(cmdline) {
			t.Errorf("pattern matches non-daemon %q", cmdline)
		}
	}
}

// The enroll step looks at the worker's data dir: a certificate means
// enrolled; a pending key without one means it already asked to join (joining
// again would leave a second pending node), anything else joins.
func TestEnrollCheckStates(t *testing.T) {
	hub := &config.Hub{ID: "hub-1", Address: "hub.example.com:7443", CAFingerprint: "sha256:aa", Mode: config.ModePublic}
	cases := []struct {
		name    string
		hubTOML string
		files   []string
		want    bool
	}{
		{"nothing", "", nil, false},
		{"other hub", `address = "other.example.com:7443"`, []string{"tls/node.crt"}, false},
		{"certificate", `address = "hub.example.com:7443"`, []string{"tls/ca.crt", "tls/node.crt", "tls/node.key"}, true},
		{"pending", `address = "hub.example.com:7443"`, []string{"tls/ca.crt", "tls/pending.key"}, true},
		{"tls dir, unknown layout", `address = "hub.example.com:7443"`, []string{"tls/other"}, true},
	}
	for _, tc := range cases {
		home := t.TempDir()
		t.Setenv("HOME", home)
		if tc.hubTOML != "" {
			writeFile(t, filepath.Join(home, ".longarm", "hub.toml"), tc.hubTOML)
		}
		for _, f := range tc.files {
			writeFile(t, filepath.Join(home, ".longarm", f), "x")
		}
		p, err := BuildWorkerAdd(WorkerRequest{Name: "worker-1", Hub: hub, Version: "v0.1.42"}, ubuntu(), WorkerDeps{Client: &fakeClient{}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := stepByID(t, p, "enroll").Check(context.Background(), plan.NewEnv())
		if err != nil || got != tc.want {
			t.Errorf("%s: check %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
}

// Approved at the hub but the worker never collected its certificate (it
// cannot reach the hub): the timeout says so and points at longarmd doctor.
func TestOnlineTimeoutNamesUncollectedCertificate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFile(t, filepath.Join(home, ".longarm", "hub.toml"), `address = "hub.example.com:7443"`)
	writeFile(t, filepath.Join(home, ".longarm", "tls", "pending.key"), "x")
	hub := &config.Hub{ID: "hub-1", Address: "hub.example.com:7443", CAFingerprint: "sha256:aa", Mode: config.ModePublic}
	client := &fakeClient{nodes: []longarm.Node{{Name: "worker-1", Role: "worker", State: "approved"}}}
	p, err := BuildWorkerAdd(WorkerRequest{Name: "worker-1", Hub: hub, Version: "v0.1.42"}, ubuntu(), WorkerDeps{Client: client, PollInterval: time.Millisecond, WaitTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	err = stepByID(t, p, "online").Apply(context.Background(), plan.NewEnv())
	var fe *plan.FixError
	if !errors.As(err, &fe) || !strings.Contains(err.Error(), "never collected its certificate") || !strings.HasSuffix(fe.Fix, "longarmd doctor") {
		t.Fatalf("%v (%+v)", err, fe)
	}

	// With a certificate it is the plain timeout, with the service's logs.
	writeFile(t, filepath.Join(home, ".longarm", "tls", "node.crt"), "x")
	err = stepByID(t, p, "online").Apply(context.Background(), plan.NewEnv())
	if err == nil || strings.Contains(err.Error(), "certificate") || !strings.Contains(err.Error(), "journalctl -u longarmd") {
		t.Fatal(err)
	}
}

func stepByID(t *testing.T, p *plan.Plan, id string) plan.Step {
	t.Helper()
	for _, s := range p.Steps {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no step %s in %s", id, ids(p))
	return plan.Step{}
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}
