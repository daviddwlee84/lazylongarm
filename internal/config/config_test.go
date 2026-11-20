package config

import (
	"path/filepath"
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
