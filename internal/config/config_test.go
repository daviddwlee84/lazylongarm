package config

import (
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
