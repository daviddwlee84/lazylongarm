package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/flows"
	"github.com/daviddwlee84/lazylongarm/internal/hostprobe"
)

// fakeHubLongarm puts a fake longarm on PATH that answers like an admin
// (`laptop`, user `laptop`) on a hub without users and one fleet worker,
// and logs every call. It returns the config and the log path.
func fakeHubLongarm(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
echo "$*" >> "` + log + `"
case "$*" in
  "version --json") echo '{"version":"v0.1.42","features":[]}' ;;
  "whoami --json") if [ -n "$FAKE_WHOAMI" ]; then printf '%s\n' "$FAKE_WHOAMI"; else echo '{"name":"laptop","role":"admin","user":"laptop","tenancy":false}'; fi ;;
  "admin user ls --json") if [ -n "$FAKE_USERS" ]; then printf '%s\n' "$FAKE_USERS"; else echo '{"users":[],"tenancy":false}'; fi ;;
  "worker ls --json") if [ -n "$FAKE_WORKERS" ]; then printf '%s\n' "$FAKE_WORKERS"; else echo '{"nodes":[{"id":"aaaa1111","name":"worker-1","role":"worker","state":"approved","online":true,"labels":{"os":"linux","gpu":"none"}}]}'; fi ;;
  "admin user add"*)
    echo K7Q2PX
    echo "client certificate alice for user alice, code expires 11-22 10:00" >&2 ;;
  *) echo "unexpected: $*" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "longarm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cfg := fmt.Sprintf(`default_hub = "hub-1"

[[hubs]]
id = "hub-1"
mode = "public"
address = "hub.example.com:7443"
ca_fingerprint = "sha256:aa"
config_dir = %q
version = "v0.1.42"
`, t.TempDir())
	return cfg, log
}

func calls(t *testing.T, log string) string {
	t.Helper()
	b, _ := os.ReadFile(log)
	return string(b)
}

func TestUserInviteRefusesWhileFleetWorkersExist(t *testing.T) {
	cfg, log := fakeHubLongarm(t)
	code, _, errOut := runWith(t, cfg, fakeTools(""), "user", "invite", "alice", "--out", filepath.Join(t.TempDir(), "i.md"))
	if code != 2 || !strings.Contains(errOut, "worker-1") || !strings.Contains(errOut, "--reenroll --owner") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.Contains(calls(t, log), "admin user add") {
		t.Fatal("added the user anyway")
	}
}

func TestUserInviteWritesPrivateFile(t *testing.T) {
	cfg, log := fakeHubLongarm(t)
	orig := releaseChecksum
	releaseChecksum = func(_ context.Context, _, tag, asset string) (string, error) { return "sum-of-" + asset, nil }
	t.Cleanup(func() { releaseChecksum = orig })
	out := filepath.Join(t.TempDir(), "alice.md")
	code, stdout, errOut := runWith(t, cfg, fakeTools(""), "user", "invite", "alice", "--out", out, "--allow-fleet-workers", "--yes", "--max-jobs", "8")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.Contains(stdout, "K7Q2PX") {
		t.Fatal("the code went to the terminal")
	}
	st, err := os.Stat(out)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("invite file: %v %v", err, st)
	}
	text, _ := os.ReadFile(out)
	for _, s := range []string{"longarm join hub.example.com:7443 K7Q2PX --ca-fingerprint sha256:aa", "`sum-of-longarmd-linux-arm64`", "V=v0.1.42"} {
		if !strings.Contains(string(text), s) {
			t.Errorf("invite lacks %q", s)
		}
	}
	if !strings.Contains(calls(t, log), "admin user add alice --ttl 24h0m0s --max-jobs=8") {
		t.Fatalf("calls:\n%s", calls(t, log))
	}
}

func TestUserInviteNeedsOutOrYesWithoutTerminal(t *testing.T) {
	cfg, log := fakeHubLongarm(t)
	code, _, errOut := runWith(t, cfg, fakeTools(""), "user", "invite", "alice", "--allow-fleet-workers")
	if code != 2 || !strings.Contains(errOut, "--out") || strings.Contains(calls(t, log), "admin user add") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

func TestUserSelfDryRun(t *testing.T) {
	cfg, log := fakeHubLongarm(t)
	code, out, errOut := runWith(t, cfg, fakeTools(""), "user", "self", "alice", "--device", "alice-laptop", "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var p struct {
		Kind  string `json:"kind"`
		Steps []struct {
			ID string `json:"id"`
		} `json:"steps"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if p.Kind != "user-self" || len(p.Steps) != 4 || !strings.Contains(strings.Join(p.Warnings, "\n"), "first user") {
		t.Fatalf("plan %+v", p)
	}
	if c := calls(t, log); strings.Contains(c, "user add") || strings.Contains(c, "join") {
		t.Fatalf("a dry run changed something:\n%s", c)
	}
}

func TestUserRemoveRefusesOwnUser(t *testing.T) {
	cfg, log := fakeHubLongarm(t)
	code, _, errOut := runWith(t, cfg, fakeTools(""), "user", "remove", "laptop", "--yes")
	if code != 2 || !strings.Contains(errOut, "revoke this machine") || strings.Contains(calls(t, log), "user remove") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

func TestWorkerAddOwnerMustBeThisMachinesUser(t *testing.T) {
	cfg, log := fakeHubLongarm(t)
	code, _, errOut := runWith(t, cfg, fakeTools(""), "worker", "add", "--local", "--name", "box-1", "--service", "user", "--owner", "alice", "--dry-run")
	if code != 2 || !strings.Contains(errOut, `acts as user "laptop"`) {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.Contains(calls(t, log), "join-code") {
		t.Fatal("minted a code during a dry run")
	}
}

// Re-enrolling an owned worker without --owner keeps its owner: it must not
// become a fleet worker every user may use.
func TestReenrollKeepsOwner(t *testing.T) {
	cfg, _ := fakeHubLongarm(t)
	t.Setenv("FAKE_WHOAMI", `{"name":"laptop-2","role":"admin","user":"alice","tenancy":true}`)
	t.Setenv("FAKE_USERS", `{"users":[{"name":"alice","device_count":1,"worker_count":1}],"tenancy":true}`)
	t.Setenv("FAKE_WORKERS", `{"nodes":[{"id":"bbbb2222cccc","name":"box-1","role":"worker","state":"approved","owner":"alice","labels":{"os":"linux","pool":"home"}}]}`)
	code, out, errOut := runWith(t, cfg, fakeTools(""), "worker", "add", "--local", "--name", "box-1", "--service", "user", "--reenroll", "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, s := range []string{"--owner alice", "owned by alice", "longarm admin revoke box-1", "--labels pool=home"} {
		if !strings.Contains(out, s) {
			t.Errorf("plan lacks %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, "every user of the hub may use it") {
		t.Fatalf("re-enroll made an owned worker a fleet worker:\n%s", out)
	}
}

// Without --name the default name (here: this machine's hostname) must be
// known before looking up the node a --reenroll replaces; otherwise the old
// node is not revoked and the join fails on the taken name.
func TestReenrollFindsNodeByDefaultName(t *testing.T) {
	cfg, _ := fakeHubLongarm(t)
	facts, err := hostprobe.Probe(context.Background(), "")
	if err != nil {
		t.Skipf("local probe: %v", err)
	}
	name := flows.WorkerDefaults(flows.WorkerRequest{}, facts).Name
	t.Setenv("FAKE_WORKERS", fmt.Sprintf(`{"nodes":[{"id":"dddd4444eeee","name":%q,"role":"worker","state":"approved"}]}`, name))
	code, out, errOut := runWith(t, cfg, fakeTools(""), "worker", "add", "--local", "--service", "user", "--reenroll", "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "longarm admin revoke "+name) {
		t.Fatalf("no revoke of %s in the plan:\n%s", name, out)
	}
}

func TestKnownTargetKeepsServiceMode(t *testing.T) {
	cfg := &config.Config{Targets: []config.Target{
		{ID: "nas", SSHHost: "nas-alias", Service: "user"},
		{ID: "here", Service: "system"},
		{ID: "odd", SSHHost: "odd", Service: "none"},
	}}
	if tg, ok := knownTarget(cfg, "nas-alias", false); !ok || tg.Service != "user" {
		t.Fatalf("nas: %+v %v", tg, ok)
	}
	if tg, ok := knownTarget(cfg, "", true); !ok || tg.ID != "here" {
		t.Fatalf("local: %+v %v", tg, ok)
	}
	if _, ok := knownTarget(cfg, "odd", false); ok {
		t.Fatal("a target without a usable service mode")
	}
	if _, ok := knownTarget(cfg, "new-box", false); ok {
		t.Fatal("unknown host matched")
	}
}
