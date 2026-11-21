package flows

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/hostprobe"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/secret"
)

// localFacts is this machine with a fake longarmd in home/.local/bin that
// logs its arguments and stdin to home/longarmd.log.
func localFacts(t *testing.T) *hostprobe.Facts {
	t.Helper()
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"longarmd $*\" >> \"" + filepath.Join(home, "longarmd.log") + "\"\ncat >> \"" + filepath.Join(home, "longarmd.log") + "\" 2>/dev/null || true\n"
	if err := os.WriteFile(filepath.Join(bin, "longarmd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f := ubuntu()
	f.Home = home
	return f
}

func TestOwnerReenrollRevokesAfterMintAndSkipsApproval(t *testing.T) {
	hub := &config.Hub{ID: "hub-1", Address: "hub.example.com:7443", CAFingerprint: "sha256:aa", Mode: config.ModePublic}
	old := longarm.Node{ID: "0ld0ld0ld-1", Name: "w1", Role: "worker", State: "approved", Online: true, Labels: map[string]string{"gpu": "none"}}
	client := &fakeClient{code: secret.New("C0DE-3")}
	f := localFacts(t)
	req := WorkerRequest{Name: "w1", Hub: hub, Version: "v0.1.42", Mint: MintAdmin, Owner: "alice", Reenroll: true, Replace: &old, Labels: []string{"gpu=none"}, Service: ServiceUser}
	p, err := BuildWorkerAdd(req, f, WorkerDeps{Client: client, PollInterval: time.Millisecond, WaitTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	p.Render(&b)
	review := b.String()
	for _, s := range []string{"--role worker --owner alice --name w1", "longarm admin revoke w1", "old node id 0ld0ld0l", "owned by alice", "new node ID", "stop the running longarmd"} {
		if !strings.Contains(review, s) {
			t.Errorf("review lacks %q:\n%s", s, review)
		}
	}
	if strings.Contains(review, "admin approve") || strings.Contains(review, "C0DE-3") {
		t.Fatalf("review approves an owned worker or shows the code:\n%s", review)
	}

	enroll := stepByID(t, p, "enroll")
	if done, _ := enroll.Check(context.Background(), plan.NewEnv()); done {
		t.Fatal("a re-enroll must not be skipped")
	}
	if err := enroll.Apply(context.Background(), plan.NewEnv()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(client.calls, ","); got != "join-code w1,revoke w1" {
		t.Fatalf("calls %s (mint before revoke, nothing else)", got)
	}
	if client.joinCodes[0].Owner != "alice" || len(client.approved) != 0 {
		t.Fatalf("code %+v approved %v", client.joinCodes[0], client.approved)
	}
	log, _ := os.ReadFile(filepath.Join(f.Home, "longarmd.log"))
	if !strings.Contains(string(log), "join hub.example.com:7443 C0DE-3 --ca-fingerprint sha256:aa --name w1 --no-wait --force") {
		t.Fatalf("longarmd log:\n%s", log)
	}

	// The hub still lists the revoked node next to the new one.
	client.nodes = []longarm.Node{old, {ID: "new-id", Name: "w1", Role: "worker", State: "approved", Online: true, Owner: "alice"}}
	client.nodes[0].Online = false
	env := plan.NewEnv()
	if err := stepByID(t, p, "online").Apply(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if len(client.approved) != 0 {
		t.Fatalf("approved an owned worker: %v", client.approved)
	}
}

func TestOwnerValidation(t *testing.T) {
	f := ubuntu()
	hub := &config.Hub{ID: "h", Address: "a:7443", CAFingerprint: "x"}
	base := WorkerRequest{Name: "w1", Hub: hub, Service: ServiceSystem, Fetch: FetchAuto, Mint: MintAdmin}
	for name, mod := range map[string]func(*WorkerRequest){
		"owner with owner mint": func(r *WorkerRequest) { r.Owner, r.Mint = "alice", MintOwner },
		"bad owner name":        func(r *WorkerRequest) { r.Owner = "Alice!" },
		"replace without flag":  func(r *WorkerRequest) { r.Replace = &longarm.Node{Name: "w1"} },
	} {
		r := base
		mod(&r)
		if err := ValidateWorker(r, f); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	r := base
	r.Owner = "alice"
	if err := ValidateWorker(r, f); err != nil {
		t.Fatalf("owner rejected: %v", err)
	}
}

func selfFixture(t *testing.T) (SelfRequest, *fakeClient) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tls"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls", "node.crt"), []byte("old cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	hub := &config.Hub{ID: "hub-1", Address: "hub.example.com:7443", CAFingerprint: "sha256:aa", Mode: config.ModePublic}
	client := &fakeClient{who: longarm.WhoAmI{Name: "laptop", Role: "admin", User: "laptop"},
		added: longarm.UserAdded{Code: secret.New("ADM1N1"), Password: secret.New("pw-123"), TOTP: secret.New("TOTPSECRET"), Otpauth: secret.New("otpauth://totp/x")}}
	return SelfRequest{Hub: hub, Name: "alice", Device: "alice-laptop", Panel: true, ConfigDir: dir}, client
}

func TestUserSelfPlan(t *testing.T) {
	req, client := selfFixture(t)
	p, err := BuildUserSelf(req, client.who, longarm.UserList{}, client)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(p); got != "backup,user,join,verify" {
		t.Fatalf("steps %s", got)
	}
	var b strings.Builder
	p.Render(&b)
	review := b.String()
	for _, s := range []string{"admin user add alice --admin --device alice-laptop --ttl 15m --panel", "first user", "lazylongarm user remove laptop", "--force", "longarm mcp"} {
		if !strings.Contains(review, s) {
			t.Errorf("review lacks %q:\n%s", s, review)
		}
	}
	env := plan.NewEnv()
	for _, s := range p.Steps {
		if s.ID == "verify" {
			client.who = longarm.WhoAmI{Name: "alice-laptop", Role: "admin", User: "alice"}
		}
		if err := s.Apply(context.Background(), env); err != nil {
			t.Fatalf("%s: %v", s.ID, err)
		}
	}
	if got := strings.Join(client.calls, ","); got != "user add alice,daemon stop,join --force,daemon start" {
		t.Fatalf("calls %s", got)
	}
	if u := client.userAdds[0]; !u.Admin || !u.Panel || u.Device != "alice-laptop" {
		t.Fatalf("user add %+v", u)
	}
	if env.Secret("panel_password").Reveal() != "pw-123" || env.Secret("panel_totp").Reveal() != "TOTPSECRET" {
		t.Fatal("panel login not kept for the summary")
	}
	backup := env.Get("backup")
	if data, err := os.ReadFile(filepath.Join(backup, "node.crt")); err != nil || string(data) != "old cert" {
		t.Fatalf("backup %s: %v %q", backup, err, data)
	}
	for _, v := range env.Values() {
		if strings.Contains(v, "ADM1N1") || strings.Contains(v, "pw-123") {
			t.Fatal("a secret leaked into the journaled values")
		}
	}
}

func TestUserSelfCases(t *testing.T) {
	req, client := selfFixture(t)
	if _, err := BuildUserSelf(req, longarm.WhoAmI{Name: "alice-laptop", Role: "admin", User: "alice"}, longarm.UserList{}, client); !errors.Is(err, ErrAlreadySelf) {
		t.Fatalf("already self: %v", err)
	}
	if _, err := BuildUserSelf(req, longarm.WhoAmI{Name: "box", Role: "client", User: "box"}, longarm.UserList{}, client); err == nil {
		t.Fatal("a client certificate may not add users")
	}
	withDevice := longarm.UserList{Tenancy: true, Users: []longarm.User{{Name: "alice", DeviceCount: 1}}}
	if _, err := BuildUserSelf(req, client.who, withDevice, client); err == nil || !strings.Contains(err.Error(), "first device only") {
		t.Fatalf("user with a device: %v", err)
	}
	// An existing user without devices (a run that stopped after adding it)
	// gets a code with `admin user code`, and no second panel login.
	noDevice := longarm.UserList{Tenancy: true, Users: []longarm.User{{Name: "alice"}}}
	p, err := BuildUserSelf(req, client.who, noDevice, client)
	if err != nil {
		t.Fatal(err)
	}
	if err := stepByID(t, p, "user").Apply(context.Background(), plan.NewEnv()); err != nil {
		t.Fatal(err)
	}
	if client.calls[0] != "user code alice" || client.userAdds[0].Panel {
		t.Fatalf("calls %v add %+v", client.calls, client.userAdds[0])
	}
	if _, err := BuildUserSelf(SelfRequest{Hub: req.Hub, Name: "Not OK"}, client.who, longarm.UserList{}, client); err == nil {
		t.Fatal("bad user name accepted")
	}
}

func TestFleetWorkers(t *testing.T) {
	nodes := []longarm.Node{
		{Name: "b", Role: "worker", State: "approved"},
		{Name: "a", Role: "worker", State: "approved"},
		{Name: "mine", Role: "worker", State: "approved", Owner: "alice"},
		{Name: "gone", Role: "worker", State: "revoked"},
		{Name: "waiting", Role: "worker", State: "pending"},
		{Name: "laptop", Role: "admin", State: "approved"},
	}
	if got := strings.Join(FleetWorkers(nodes), ","); got != "a,b" {
		t.Fatalf("fleet workers %s", got)
	}
}

func TestRenderInvite(t *testing.T) {
	inv := Invite{User: "alice", HubAddress: "hub.example.com:7443", Fingerprint: "sha256:aa", Code: secret.New("J01N42"),
		Expires: time.Date(2030, 1, 2, 3, 4, 0, 0, time.UTC), Version: "v0.1.42", Repo: "owner/longarm",
		Checksums: map[string]string{"longarm-darwin-arm64": "abc", "longarmd-darwin-arm64": "def"}}
	text := RenderInvite(inv)
	for _, s := range []string{
		"longarm join hub.example.com:7443 J01N42 --ca-fingerprint sha256:aa",
		"longarm worker add <machine-name>",
		"longarmd join hub.example.com:7443 <worker-code> --ca-fingerprint sha256:aa --name <machine-name>",
		"releases/download/$V/longarmd-$P",
		"| `longarmd-darwin-arm64` | `def` |",
		`owners = ["alice"]`,
		"hub_job_details = false",
	} {
		if !strings.Contains(text, s) {
			t.Errorf("invite lacks %q", s)
		}
	}
	if strings.Contains(text, "Web panel") {
		t.Fatal("panel section without a panel login")
	}
	inv.Checksums, inv.Password, inv.TOTP = nil, secret.New("pw"), secret.New("T0TP")
	text = RenderInvite(inv)
	if !strings.Contains(text, "SHA256SUMS") || !strings.Contains(text, "password `pw`") {
		t.Fatalf("fallbacks:\n%s", text)
	}
}

// Without a panel login made now, the user's later devices can only be
// approved by an admin panel login named like it: the review says so.
func TestUserSelfWarnsWithoutPanel(t *testing.T) {
	req, client := selfFixture(t)
	warned := func(r SelfRequest, users longarm.UserList) bool {
		t.Helper()
		p, err := BuildUserSelf(r, client.who, users, client)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Contains(strings.Join(p.Warnings, "\n"), "lazylongarm hub panel-user alice")
	}
	if warned(req, longarm.UserList{}) {
		t.Fatal("warned although --panel makes the login")
	}
	noPanel := req
	noPanel.Panel = false
	if !warned(noPanel, longarm.UserList{}) {
		t.Fatal("no warning without --panel")
	}
	// an existing user (no device yet) gets no second login: warn unless it has one
	if !warned(req, longarm.UserList{Tenancy: true, Users: []longarm.User{{Name: "alice"}}}) {
		t.Fatal("no warning for an existing user without a panel login")
	}
	if warned(req, longarm.UserList{Tenancy: true, Users: []longarm.User{{Name: "alice", Panel: true}}}) {
		t.Fatal("warned although the user has a panel login")
	}
}
