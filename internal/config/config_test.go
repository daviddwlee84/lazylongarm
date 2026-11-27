package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"
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

// A newer lazylongarm wrote fields this build does not know. Loading works
// and reports them; writing keeps them.
const newerConfig = `# written by a newer lazylongarm
default_hub = "hub"
future_flag = true

[future_section]
  level = 3

[[hubs]]
  id = "hub"
  mode = "public"
  address = "hub.example.com:7443"
  region = "west"

  [hubs.cloud]
    provider = "azure"
    zone = "1"

[[targets]]
  id = "worker-1"
  hub = "hub"
  pool_name = "c1"

[[targets]]
  id = "worker-2"
  hub = "hub"
  pool_name = "c2"

[tui]
  theme = "dark"
`

func writeConfig(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func rawConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw := map[string]any{}
	if err := toml.Unmarshal(data, &raw); err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	return raw
}

func entry(raw map[string]any, list string, i int) map[string]any {
	arr, _ := raw[list].([]any)
	if i >= len(arr) {
		return nil
	}
	m, _ := arr[i].(map[string]any)
	return m
}

func TestUnknownKeysLoadWithWarnings(t *testing.T) {
	c, err := Load(writeConfig(t, newerConfig), true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"future_flag", "future_section", "hubs[hub].cloud.zone", "hubs[hub].region", "targets[worker-1].pool_name", "targets[worker-2].pool_name", "tui.theme"}
	if got := c.UnknownKeys(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("unknown keys\n got %q\nwant %q", got, want)
	}
	if len(c.Targets) != 2 || c.Hubs[0].Cloud == nil || c.Hubs[0].Cloud.Provider != "azure" {
		t.Fatalf("known fields not loaded: %+v", c)
	}
	// An entry without an id (validation rejects that today) shows its index.
	if p := (unknownKey{at: []step{{key: "targets", entry: true, index: 2}}, key: "pool_name"}).path(); p != "targets[2].pool_name" {
		t.Fatal(p)
	}
}

func TestUpdateKeepsUnknownKeys(t *testing.T) {
	path := writeConfig(t, newerConfig)
	c, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Update(func(c *Config) error {
		c.UpsertTarget(Target{ID: "worker-3", Hub: "hub"})
		c.TUI.RefreshSeconds = 9
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw := rawConfig(t, path)
	if raw["future_flag"] != true || raw["future_section"].(map[string]any)["level"] != int64(3) {
		t.Fatalf("top-level keys lost: %v", raw)
	}
	hub := entry(raw, "hubs", 0)
	if hub["region"] != "west" || hub["cloud"].(map[string]any)["zone"] != "1" {
		t.Fatalf("hub keys lost: %v", hub)
	}
	if entry(raw, "targets", 0)["pool_name"] != "c1" || entry(raw, "targets", 1)["pool_name"] != "c2" || entry(raw, "targets", 2)["pool_name"] != nil {
		t.Fatalf("target keys misplaced: %v", raw["targets"])
	}
	if tui := raw["tui"].(map[string]any); tui["theme"] != "dark" || tui["refresh_seconds"] != int64(9) {
		t.Fatalf("tui: %v", tui)
	}
	// The written file still loads, with the same unknown keys, and a
	// whole-document Save keeps them too.
	again, err := Load(path, true)
	if err != nil || len(again.UnknownKeys()) != 7 || len(again.Targets) != 3 {
		t.Fatalf("%v %q", err, again.UnknownKeys())
	}
	if err := again.Save(); err != nil {
		t.Fatal(err)
	}
	if entry(rawConfig(t, path), "targets", 1)["pool_name"] != "c2" {
		t.Fatal("Save dropped unknown keys")
	}
}

func TestDeletingEntryDropsItsUnknownKeys(t *testing.T) {
	path := writeConfig(t, newerConfig)
	c, _ := Load(path, true)
	if err := c.Update(func(c *Config) error {
		c.Targets = c.Targets[:1] // remove worker-2
		c.Hubs[0].Cloud = nil     // and the hub's cloud table
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw := rawConfig(t, path)
	if arr := raw["targets"].([]any); len(arr) != 1 || entry(raw, "targets", 0)["pool_name"] != "c1" {
		t.Fatalf("targets: %v", raw["targets"])
	}
	if hub := entry(raw, "hubs", 0); hub["cloud"] != nil || hub["region"] != "west" {
		t.Fatalf("hub: %v", hub)
	}
	if got := c.UnknownKeys(); len(got) != 5 {
		t.Fatalf("in-memory config not refreshed: %q", got)
	}
	// Removing the hub removes its targets and every key under them.
	if err := c.Update(func(c *Config) error { c.RemoveHub("hub"); return nil }); err != nil {
		t.Fatal(err)
	}
	raw = rawConfig(t, path)
	if raw["hubs"] != nil || raw["targets"] != nil || raw["future_flag"] != true || raw["tui"].(map[string]any)["theme"] != "dark" {
		t.Fatalf("after RemoveHub: %v", raw)
	}
}

// go-toml matches field names case-insensitively; such a key is known and
// comes back under its canonical name, not twice.
func TestCaseVariantKeyIsKnown(t *testing.T) {
	path := writeConfig(t, "[[targets]]\nID = \"worker-1\"\nName = \"one\"\n")
	c, err := Load(path, true)
	if err != nil || len(c.UnknownKeys()) != 0 || c.Targets[0].Name != "one" {
		t.Fatalf("%v %q %+v", err, c.UnknownKeys(), c.Targets)
	}
	if err := c.Update(func(*Config) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if tg := entry(rawConfig(t, path), "targets", 0); tg["name"] != "one" || tg["Name"] != nil {
		t.Fatalf("%v", tg)
	}
}

func TestInvalidConfigStillFails(t *testing.T) {
	for name, tc := range map[string]struct{ data, want string }{
		"type mismatch":  {"[[hubs]]\nid = \"hub\"\nmode = 3\naddress = \"hub.example.com:7443\"\n", "line 3"},
		"nested type":    {"[tui]\nrefresh_seconds = \"fast\"\n", "RefreshSeconds"},
		"broken toml":    {"[tui\n", "parse"},
		"validation":     {"[[hubs]]\nid = \"hub\"\nmode = \"sideways\"\naddress = \"x:1\"\nregion = \"west\"\n", "unknown mode"},
		"dangling hub":   {"[[targets]]\nid = \"worker-1\"\nhub = \"nope\"\npool_name = \"c\"\n", "unknown hub"},
		"duplicate keys": {"default_hub = \"a\"\ndefault_hub = \"b\"\n", "parse"},
	} {
		if _, err := Load(writeConfig(t, tc.data), true); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, tc.want)
		}
	}
}

func TestPanelAccessRoundTripAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	c, _ := Load(path, false)
	c.UpsertHub(Hub{ID: "h", Mode: ModePublic, Address: "hub.example.com:7443"})
	if got := c.Hubs[0].PanelMode(); got != PanelLoopback {
		t.Fatalf("default panel mode %q", got)
	}
	c.Hubs[0].PanelAccess = &PanelAccess{Mode: PanelPublic, URL: "https://hub.example.com/", Domain: "hub.example.com", Redirect: true}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	h := loaded.Hubs[0]
	if h.PanelMode() != PanelPublic || h.PanelURL() != "https://hub.example.com/" || !h.PanelAccess.Redirect {
		t.Fatalf("%+v", h.PanelAccess)
	}
	loaded.Hubs[0].PanelAccess = &PanelAccess{Mode: PanelPublic}
	if err := loaded.Validate(); err == nil || !strings.Contains(err.Error(), "domain") {
		t.Fatalf("public without domain accepted: %v", err)
	}
	loaded.Hubs[0].PanelAccess = &PanelAccess{Mode: "funnel"}
	if err := loaded.Validate(); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

func TestFerryStateRoundTripAndSurvivesUpsert(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	c, _ := Load(path, false)
	c.UpsertTarget(Target{ID: "w1", Name: "w1", SSHHost: "worker-1"})
	c.Targets[0].FerryVersion, c.Targets[0].FerryBackends = "0.2.0", []string{"baidupcs"}
	c.UpsertTarget(Target{ID: "w1", Name: "w1", SSHHost: "worker-1", Service: "system"}) // worker add rerun
	if got := c.Targets[0]; got.FerryVersion != "0.2.0" || len(got.FerryBackends) != 1 {
		t.Fatalf("upsert lost ferry state: %+v", got)
	}
	c.Companions.Ferry.UpsertJob(FerryJob{Name: "photos", Sender: "w1", Receiver: "w2", Source: "/data/photos", Dest: "/srv/in", Adapter: "baidupcs", Prefix: "/ferry/photos", CreatedAt: "2026-01-01T00:00:00Z"})
	c.Companions.Ferry.UpsertJob(FerryJob{Name: "logs", Sender: "w2", Receiver: "w1", Source: "/var/log/x", Dest: "/srv/logs", Adapter: "rclone", Prefix: "/ferry/logs"})
	c.Companions.Ferry.UpsertJob(FerryJob{Name: "photos", Sender: "w1", Receiver: "w2", Source: "/data/photos", Dest: "/srv/in2", Adapter: "baidupcs", Prefix: "/ferry/photos"})
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	jobs := loaded.Companions.Ferry.Jobs
	if len(jobs) != 2 || jobs[0].Name != "logs" || jobs[1].Dest != "/srv/in2" || jobs[1].CreatedAt != "2026-01-01T00:00:00Z" {
		t.Fatalf("jobs %+v", jobs)
	}
	if loaded.Targets[0].FerryBackends[0] != "baidupcs" {
		t.Fatalf("%+v", loaded.Targets[0])
	}
}
