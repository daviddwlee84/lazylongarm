package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoveHubDropsTargetsAndDefault(t *testing.T) {
	c := &Config{path: filepath.Join(t.TempDir(), "c.toml")}
	c.UpsertHub(Hub{ID: "a", Mode: ModePublic, Address: "a:7443"})
	c.UpsertHub(Hub{ID: "b", Mode: ModePrivate, Address: "b:7443"})
	c.UpsertTarget(Target{ID: "w1", Hub: "a"})
	c.UpsertTarget(Target{ID: "w2", Hub: "b"})
	c.RemoveHub("a")
	if len(c.Hubs) != 1 || c.Hubs[0].ID != "b" || c.DefaultHub != "b" || len(c.Targets) != 1 || c.Targets[0].ID != "w2" {
		t.Fatalf("%+v", c)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(c.path, true)
	if err != nil || loaded.DefaultHub != "b" {
		t.Fatalf("%v %+v", err, loaded)
	}
}

func TestSaveDetectsConcurrentEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	a, _ := Load(path, false)
	a.UpsertHub(Hub{ID: "x", Mode: ModePublic, Address: "x:1"})
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := Load(path, true)
	a.UpsertHub(Hub{ID: "y", Mode: ModePublic, Address: "y:1"})
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	b.UpsertHub(Hub{ID: "z", Mode: ModePublic, Address: "z:1"})
	if err := b.Save(); err != ErrConflict {
		t.Fatalf("stale save: %v", err)
	}
}

// Two long-running commands (e.g. `worker add` on two machines) load the
// config at startup and each record their own target minutes later.
func TestUpdateKeepsConcurrentTargets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	seed, _ := Load(path, false)
	if err := seed.Update(func(c *Config) error {
		c.UpsertHub(Hub{ID: "hub", Mode: ModePublic, Address: "hub.example.com:7443"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a, _ := Load(path, true)
	b, _ := Load(path, true)
	add := func(c *Config, id string) error {
		return c.Update(func(c *Config) error {
			c.UpsertTarget(Target{ID: id, Name: id, SSHHost: id, Hub: "hub"})
			return nil
		})
	}
	if err := add(a, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if err := add(b, "worker-2"); err != nil {
		t.Fatalf("second writer: %v", err)
	}
	loaded, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Targets) != 2 || loaded.Targets[0].ID != "worker-1" || loaded.Targets[1].ID != "worker-2" {
		t.Fatalf("targets %+v", loaded.Targets)
	}
	// b's in-memory config follows the file, so it can keep saving.
	if _, ok := b.TargetByName("worker-1"); !ok || len(b.Targets) != 2 {
		t.Fatalf("b not refreshed: %+v", b.Targets)
	}
	if err := b.Save(); err != nil {
		t.Fatalf("save after update: %v", err)
	}
	// Same entry: the last writer wins for that target, others survive.
	if err := a.Update(func(c *Config) error {
		c.UpsertTarget(Target{ID: "worker-2", Name: "worker-2", SSHHost: "worker-2", Hub: "hub", Service: "system"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	loaded, _ = Load(path, true)
	if w2, _ := loaded.TargetByName("worker-2"); len(loaded.Targets) != 2 || w2.Service != "system" {
		t.Fatalf("same-entry update: %+v", loaded.Targets)
	}
}

// A stale whole-document Save conflicts; recording the change again through
// Update (what a step retry does) succeeds and keeps the other edit.
func TestUpdateRetryAfterConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	stale, _ := Load(path, false)
	other, _ := Load(path, false)
	if err := other.Update(func(c *Config) error {
		c.UpsertTarget(Target{ID: "worker-1"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stale.UpsertTarget(Target{ID: "worker-2"})
	if err := stale.Save(); err != ErrConflict {
		t.Fatalf("stale save: %v", err)
	}
	for i := 0; i < 2; i++ { // idempotent: retrying again is harmless
		if err := stale.Update(func(c *Config) error {
			c.UpsertTarget(Target{ID: "worker-2"})
			return nil
		}); err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
	}
	loaded, _ := Load(path, true)
	if len(loaded.Targets) != 2 || loaded.Targets[0].ID != "worker-1" || loaded.Targets[1].ID != "worker-2" {
		t.Fatalf("targets %+v", loaded.Targets)
	}
}

func TestUpdateErrorWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	c, _ := Load(path, false)
	boom := errors.New("boom")
	if err := c.Update(func(c *Config) error {
		c.UpsertTarget(Target{ID: "worker-1"})
		return boom
	}); err != boom {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) || len(c.Targets) != 0 {
		t.Fatalf("written or changed: %v %+v", err, c.Targets)
	}
	// An invalid result is refused too.
	if err := c.Update(func(c *Config) error {
		c.UpsertTarget(Target{ID: "worker-1", Hub: "missing"})
		return nil
	}); err == nil {
		t.Fatal("invalid config written")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("invalid config written: %v", err)
	}
}

func TestCompanionFieldsRoundTripAndSurviveUpsert(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	c, _ := Load(path, false)
	c.Companions.Dotfiles = Dotfiles{Repo: "https://github.com/you/dotfiles.git", DefaultBundle: "server", BundleByOS: map[string]string{"darwin": "laptop"}}
	c.UpsertTarget(Target{ID: "w1", Name: "w1", SSHHost: "worker-1"})
	c.Targets[0].DotfilesBundle, c.Targets[0].DotfilesRev = "server", "abc1234"
	added := c.Targets[0].AddedAt
	c.UpsertTarget(Target{ID: "w1", Name: "w1", SSHHost: "worker-1", Service: "system"}) // worker add rerun
	if got := c.Targets[0]; got.DotfilesRev != "abc1234" || got.AddedAt != added || got.Service != "system" {
		t.Fatalf("upsert lost state: %+v", got)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Companions.Dotfiles.BundleByOS["darwin"] != "laptop" || loaded.Targets[0].DotfilesBundle != "server" {
		t.Fatalf("%+v", loaded)
	}
	if tg, ok := loaded.TargetByName("w1"); !ok || tg.SSHHost != "worker-1" {
		t.Fatalf("TargetByName: %v %+v", ok, tg)
	}
}

func TestEmptyCompanionsAreNotWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	c, _ := Load(path, false)
	c.UpsertHub(Hub{ID: "x", Mode: ModePublic, Address: "x:1"})
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "companions") {
		t.Fatalf("empty companions table written:\n%s", data)
	}
}

func TestPresetsDecodeTypedValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	data := "[companions.dotfiles]\nanswers = \"server\"\n[companions.dotfiles.presets.server]\nprofile = \"ubuntu_server\"\ninstallTailscale = true\njobs = 4\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Companions.Dotfiles.Presets["server"]
	if c.Companions.Dotfiles.Answers != "server" || p["profile"] != "ubuntu_server" || p["installTailscale"] != true || p["jobs"] != int64(4) {
		t.Fatalf("%#v", c.Companions.Dotfiles)
	}
}

func TestUpsertKeepsClientName(t *testing.T) {
	c, _ := Load(filepath.Join(t.TempDir(), "c.toml"), false)
	c.UpsertTarget(Target{ID: "w1", Name: "w1", SSHHost: "worker-1", ClientName: "w1-client"}) // peer add
	c.UpsertTarget(Target{ID: "w1", Name: "w1", SSHHost: "worker-1", Service: "user"})         // worker add rerun
	if got := c.Targets[0]; got.ClientName != "w1-client" || got.Service != "user" {
		t.Fatalf("upsert lost the client: %+v", got)
	}
}
