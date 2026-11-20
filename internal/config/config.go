// Package config owns lazylongarm's preferences and registrations: hubs it
// set up or joined and the machines (targets) it manages. Longarm's own
// state stays in Longarm's directories; this file only references it.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// ErrConflict means the file changed on disk after it was loaded.
var ErrConflict = errors.New("config file changed since it was loaded; reload and retry")

// Hub modes.
const (
	ModePublic   = "public"   // hub has a public DNS name / IP; nodes dial <fqdn>:7443
	ModePrivate  = "private"  // hub reachable only on the user's private network (e.g. a tailnet IP)
	ModeExisting = "existing" // joined a hub someone else operates
)

// Config is the whole file.
type Config struct {
	DefaultHub string     `toml:"default_hub,omitempty"`
	Hubs       []Hub      `toml:"hubs,omitempty"`
	Targets    []Target   `toml:"targets,omitempty"`
	TUI        TUI        `toml:"tui,omitempty"`
	Companions Companions `toml:"companions,omitempty"`

	path   string
	digest string
}

// Hub is a longarm-hub this machine is (or will be) enrolled with.
type Hub struct {
	ID            string `toml:"id"`
	Name          string `toml:"name,omitempty"`
	Mode          string `toml:"mode"`
	Address       string `toml:"address"`                  // host:port nodes dial (public_addr)
	CAFingerprint string `toml:"ca_fingerprint,omitempty"` // sha256:... from longarm-hub
	SSHHost       string `toml:"ssh_host,omitempty"`       // SSH alias of the hub machine (when we operate it)
	Panel         string `toml:"panel,omitempty"`          // panel listen address on the hub host, e.g. 127.0.0.1:7480
	ConfigDir     string `toml:"config_dir,omitempty"`     // LONGARM_CONFIG_DIR for this hub; empty = Longarm default (~/.config/longarm)
	Version       string `toml:"version,omitempty"`        // Longarm release used for workers we install
	Cloud         *Cloud `toml:"cloud,omitempty"`
	CreatedAt     string `toml:"created_at,omitempty"`
}

// Cloud records the firewall resources lazylongarm changed for a hub, so
// status can verify and uninstall can remove exactly those rules.
type Cloud struct {
	Provider      string   `toml:"provider"` // "azure"
	Subscription  string   `toml:"subscription,omitempty"`
	ResourceGroup string   `toml:"resource_group,omitempty"`
	VM            string   `toml:"vm,omitempty"`
	NSG           string   `toml:"nsg,omitempty"`
	Rules         []string `toml:"rules,omitempty"`
}

// Target is a machine lazylongarm manages as a Longarm worker. An empty
// SSHHost is this machine.
type Target struct {
	ID         string `toml:"id"`
	Name       string `toml:"name,omitempty"`
	SSHHost    string `toml:"ssh_host,omitempty"`
	Hub        string `toml:"hub,omitempty"`
	WorkerName string `toml:"worker_name,omitempty"`
	OS         string `toml:"os,omitempty"`
	Arch       string `toml:"arch,omitempty"`
	Install    string `toml:"install,omitempty"` // "push" (lazylongarm copied binaries to ~/.local/bin) | "existing"
	Service    string `toml:"service,omitempty"` // "system" | "none"
	AddedAt    string `toml:"added_at,omitempty"`
	// ClientName is the machine's Longarm client node (`peer add`): its
	// longarm can dispatch to other workers. "" = worker only.
	ClientName string `toml:"client_name,omitempty"`

	// Companion state, written by `lazylongarm companion dotfiles`.
	DotfilesBundle    string `toml:"dotfiles_bundle,omitempty"`     // bundle used (or to use) for this machine
	DotfilesRev       string `toml:"dotfiles_rev,omitempty"`        // source revision seen after the last bootstrap
	DotfilesAppliedAt string `toml:"dotfiles_applied_at,omitempty"` // RFC 3339
}

// Companions configures the tools lazylongarm sets up next to Longarm on
// fleet machines. Personal values (repository, bundles) live only here.
type Companions struct {
	Dotfiles Dotfiles `toml:"dotfiles,omitempty"`
}

// Dotfiles configures `lazylongarm companion dotfiles`.
type Dotfiles struct {
	// Repo is the chezmoi source repository (URL, "user" or "user/repo").
	Repo string `toml:"repo,omitempty"`
	// Bootstrap is an optional shell command template run on the machine
	// instead of the built-in chezmoi install + init. Placeholders {repo},
	// {bundle}, {name}, {os}, {arch} and {distro} are replaced by
	// shell-quoted values.
	Bootstrap string `toml:"bootstrap,omitempty"`
	// DefaultBundle is used when neither the command line, the target nor
	// BundleByOS names one.
	DefaultBundle string `toml:"default_bundle,omitempty"`
	// BundleByOS maps a probed OS ("linux", "darwin") to a bundle.
	BundleByOS map[string]string `toml:"bundle_by_os,omitempty"`
	// Answers is how the built-in bootstrap answers chezmoi's prompts:
	// "ask" (in the terminal), "defaults", or a preset name. Empty = a
	// preset named like the machine's bundle, else ask.
	Answers string `toml:"answers,omitempty"`
	// Presets pre-answer chezmoi prompts: preset name → prompt text (the
	// question as the template's prompt*Once call words it, not the data
	// key) → value (string, integer or boolean).
	Presets map[string]map[string]any `toml:"presets,omitempty"`
}

// TUI preferences.
type TUI struct {
	RefreshSeconds int  `toml:"refresh_seconds,omitempty"`
	Mouse          bool `toml:"mouse,omitempty"`
}

// Path returns the file this config was loaded from or will be saved to.
func (c *Config) Path() string { return c.path }

// Load reads path. A missing file yields an empty config unless explicit.
func Load(path string, explicit bool) (*Config, error) {
	c := &Config{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if explicit {
			return nil, fmt.Errorf("config file %s does not exist", path)
		}
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields().Decode(c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.path = path
	c.digest = digest(data)
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Validate checks identity and references.
func (c *Config) Validate() error {
	hubs := map[string]bool{}
	for _, h := range c.Hubs {
		if h.ID == "" || hubs[h.ID] {
			return fmt.Errorf("hub ids must be unique and non-empty (%q)", h.ID)
		}
		hubs[h.ID] = true
		switch h.Mode {
		case ModePublic, ModePrivate, ModeExisting:
		default:
			return fmt.Errorf("hub %s: unknown mode %q", h.ID, h.Mode)
		}
		if h.Address == "" {
			return fmt.Errorf("hub %s: address is required", h.ID)
		}
	}
	if c.DefaultHub != "" && !hubs[c.DefaultHub] {
		return fmt.Errorf("default_hub %q is not a configured hub", c.DefaultHub)
	}
	targets := map[string]bool{}
	for _, t := range c.Targets {
		if t.ID == "" || targets[t.ID] {
			return fmt.Errorf("target ids must be unique and non-empty (%q)", t.ID)
		}
		targets[t.ID] = true
		if t.Hub != "" && !hubs[t.Hub] {
			return fmt.Errorf("target %s references unknown hub %q", t.ID, t.Hub)
		}
	}
	return nil
}

// Hub returns the hub with id, or the default hub when id is empty.
func (c *Config) Hub(id string) (*Hub, bool) {
	if id == "" {
		id = c.DefaultHub
	}
	if id == "" && len(c.Hubs) > 0 {
		id = c.Hubs[0].ID
	}
	for i := range c.Hubs {
		if c.Hubs[i].ID == id {
			return &c.Hubs[i], true
		}
	}
	return nil, false
}

// UpsertHub inserts or replaces h by ID; the first hub becomes the default.
func (c *Config) UpsertHub(h Hub) {
	for i := range c.Hubs {
		if c.Hubs[i].ID == h.ID {
			c.Hubs[i] = h
			return
		}
	}
	if h.CreatedAt == "" {
		h.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	c.Hubs = append(c.Hubs, h)
	if c.DefaultHub == "" {
		c.DefaultHub = h.ID
	}
}

// RemoveHub deletes a hub and the targets that reference it.
func (c *Config) RemoveHub(id string) {
	hubs := c.Hubs[:0]
	for _, h := range c.Hubs {
		if h.ID != id {
			hubs = append(hubs, h)
		}
	}
	c.Hubs = hubs
	targets := c.Targets[:0]
	for _, t := range c.Targets {
		if t.Hub != id {
			targets = append(targets, t)
		}
	}
	c.Targets = targets
	if c.DefaultHub == id {
		c.DefaultHub = ""
		if len(c.Hubs) > 0 {
			c.DefaultHub = c.Hubs[0].ID
		}
	}
}

// UpsertTarget inserts or replaces t by ID. Replacing keeps the original
// added_at, client name and the companion state when t leaves them empty, so
// rerunning `worker add` does not forget what `peer add` or `companion
// dotfiles` recorded.
func (c *Config) UpsertTarget(t Target) {
	for i := range c.Targets {
		if c.Targets[i].ID == t.ID {
			old := c.Targets[i]
			if t.AddedAt == "" {
				t.AddedAt = old.AddedAt
			}
			if t.ClientName == "" {
				t.ClientName = old.ClientName
			}
			if t.DotfilesBundle == "" && t.DotfilesRev == "" && t.DotfilesAppliedAt == "" {
				t.DotfilesBundle, t.DotfilesRev, t.DotfilesAppliedAt = old.DotfilesBundle, old.DotfilesRev, old.DotfilesAppliedAt
			}
			c.Targets[i] = t
			return
		}
	}
	if t.AddedAt == "" {
		t.AddedAt = time.Now().UTC().Format(time.RFC3339)
	}
	c.Targets = append(c.Targets, t)
	sort.SliceStable(c.Targets, func(i, j int) bool { return c.Targets[i].ID < c.Targets[j].ID })
}

// TargetByName finds a target by id or name.
func (c *Config) TargetByName(name string) (*Target, bool) {
	for i := range c.Targets {
		if c.Targets[i].ID == name || c.Targets[i].Name == name {
			return &c.Targets[i], true
		}
	}
	return nil, false
}

// TargetByHost finds the target managed through sshHost ("" = local).
func (c *Config) TargetByHost(sshHost string) (*Target, bool) {
	for i := range c.Targets {
		if c.Targets[i].SSHHost == sshHost {
			return &c.Targets[i], true
		}
	}
	return nil, false
}

// Save writes atomically with a lock, refusing to overwrite edits made by
// another process since Load (ErrConflict).
func (c *Config) Save() error {
	if c.path == "" {
		return errors.New("config path is unknown")
	}
	if err := c.Validate(); err != nil {
		return err
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	unlock, err := lockFile(c.path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	current, err := os.ReadFile(c.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if c.digest != "" {
			return ErrConflict
		}
	case err != nil:
		return err
	default:
		if digest(current) != c.digest {
			return ErrConflict
		}
	}
	var buf bytes.Buffer
	buf.WriteString("# lazylongarm configuration (written by lazylongarm; safe to edit while it is not running)\n")
	enc := toml.NewEncoder(&buf)
	enc.SetIndentTables(true)
	if err := enc.Encode(c); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), c.path); err != nil {
		return err
	}
	c.digest = digest(buf.Bytes())
	return nil
}

// Slug turns a display name into an identifier.
func Slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
