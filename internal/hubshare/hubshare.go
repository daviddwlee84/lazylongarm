// Package hubshare registers a hub this machine operates on another operator
// machine (`hub share`, `hub import`): the payload that travels between the
// two, how it merges into a config, and the identity checks (address + CA
// fingerprint) that keep a machine from adopting the wrong hub. Nothing here
// is secret: certificates and keys never leave their machine.
package hubshare

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/daviddwlee84/lazylongarm/internal/config"
	toml "github.com/pelletier/go-toml/v2"
)

// PayloadVersion is the payload format; Decode refuses newer ones.
const PayloadVersion = 1

// Payload is what `hub share` sends to `hub import --stdin`.
type Payload struct {
	Version int             `toml:"version"`
	From    string          `toml:"from,omitempty"` // the sharing machine's hostname, for the record
	Hub     config.Hub      `toml:"hub"`
	Targets []config.Target `toml:"targets,omitempty"`
}

// NewPayload copies h (and targets) without the fields that belong to one
// machine: its Longarm config dir and when it registered the hub.
func NewPayload(h config.Hub, targets []config.Target, from string) Payload {
	h.ConfigDir, h.CreatedAt = "", ""
	out := make([]config.Target, 0, len(targets))
	for _, t := range targets {
		t.AddedAt = ""
		out = append(out, t)
	}
	return Payload{Version: PayloadVersion, From: from, Hub: h, Targets: out}
}

// Encode writes p as TOML.
func (p Payload) Encode() ([]byte, error) {
	var b bytes.Buffer
	enc := toml.NewEncoder(&b)
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Decode reads a payload and checks it is one this build understands.
func Decode(b []byte) (Payload, error) {
	var p Payload
	if err := toml.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("read hub payload: %w", err)
	}
	switch {
	case p.Version == 0:
		return p, errors.New("not a hub payload (no version): pipe the output of `lazylongarm hub share`")
	case p.Version > PayloadVersion:
		return p, fmt.Errorf("hub payload version %d is newer than this lazylongarm understands (%d): upgrade lazylongarm here", p.Version, PayloadVersion)
	case p.Hub.ID == "" || p.Hub.Address == "":
		return p, errors.New("hub payload has no hub id or address")
	}
	p.Hub.CAFingerprint = NormalizeFingerprint(p.Hub.CAFingerprint)
	return p, nil
}

// NormalizeFingerprint lowercases a CA fingerprint and drops a "sha256:"
// prefix and colons, so values from longarm-hub, longarm and here compare.
func NormalizeFingerprint(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "sha256:")
	return strings.ReplaceAll(s, ":", "")
}

// CAFingerprint is Longarm's CA fingerprint of a PEM certificate: the
// SHA-256 of its DER, hex.
func CAFingerprint(pemBytes []byte) (string, error) {
	for {
		var blk *pem.Block
		blk, pemBytes = pem.Decode(pemBytes)
		if blk == nil {
			return "", errors.New("no PEM certificate found")
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return "", fmt.Errorf("parse CA certificate: %w", err)
		}
		sum := sha256.Sum256(cert.Raw)
		return hex.EncodeToString(sum[:]), nil
	}
}

// LongarmDir is the client's Longarm config dir: dir, else
// $LONGARM_CONFIG_DIR, else ~/.config/longarm.
func LongarmDir(dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	if d := os.Getenv("LONGARM_CONFIG_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "longarm"), nil
}

// Membership is the hub a Longarm client is joined to, from its config dir.
type Membership struct {
	Address       string // hub.toml address
	CAFingerprint string // of tls/ca.crt, normalized
}

// ParseMembership reads hub.toml and tls/ca.crt contents (either may be
// empty when the machine has not joined).
func ParseMembership(hubTOML, caPEM []byte) (Membership, error) {
	var m Membership
	if len(bytes.TrimSpace(hubTOML)) > 0 {
		var v struct {
			Address string `toml:"address"`
		}
		if err := toml.Unmarshal(hubTOML, &v); err != nil {
			return m, fmt.Errorf("read longarm hub.toml: %w", err)
		}
		m.Address = v.Address
	}
	if len(bytes.TrimSpace(caPEM)) > 0 {
		fp, err := CAFingerprint(caPEM)
		if err != nil {
			return m, err
		}
		m.CAFingerprint = fp
	}
	return m, nil
}

// LocalMembership reads this machine's Longarm client config dir.
func LocalMembership(dir string) (Membership, error) {
	dir, err := LongarmDir(dir)
	if err != nil {
		return Membership{}, err
	}
	hubTOML, err := os.ReadFile(filepath.Join(dir, "hub.toml"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Membership{}, err
	}
	ca, err := os.ReadFile(filepath.Join(dir, "tls", "ca.crt"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Membership{}, err
	}
	return ParseMembership(hubTOML, ca)
}

// Joined reports whether the client has both an address and a CA.
func (m Membership) Joined() bool { return m.Address != "" && m.CAFingerprint != "" }

// Matches reports whether m is joined to hub h. The CA decides when h
// records one; the address must match too, because a hub reached under
// another name is still worth a look.
func (m Membership) Matches(h config.Hub) error {
	if !m.Joined() {
		return errors.New("its longarm is not joined to a hub (no hub.toml address or tls/ca.crt)")
	}
	if fp := NormalizeFingerprint(h.CAFingerprint); fp != "" && fp != m.CAFingerprint {
		return fmt.Errorf("its longarm trusts CA %s, hub %s has CA %s: another hub", short(m.CAFingerprint), h.ID, short(fp))
	}
	if !strings.EqualFold(m.Address, h.Address) {
		return fmt.Errorf("its longarm dials %s, hub %s is at %s", m.Address, h.ID, h.Address)
	}
	return nil
}

func short(fp string) string {
	if len(fp) > 16 {
		return fp[:16] + "…"
	}
	return fp
}

// MergeResult says what Merge changed.
type MergeResult struct {
	HubID          string   `json:"hub_id"`
	Hub            string   `json:"hub"`     // added | updated | unchanged
	Default        bool     `json:"default"` // the hub is (now) the default
	TargetsAdded   []string `json:"targets_added,omitempty"`
	TargetsUpdated []string `json:"targets_updated,omitempty"`
}

// Merge registers p's hub (and targets) in c. A hub with the same address
// and CA is updated whatever its id; an id that names a different hub is
// refused (pass another id). Machine-local fields of an existing entry
// (config dir, created) are kept, and targets point at the merged hub.
func Merge(c *config.Config, p Payload) (MergeResult, error) {
	in := p.Hub
	in.CAFingerprint = NormalizeFingerprint(in.CAFingerprint)
	res := MergeResult{HubID: in.ID, Hub: "added"}
	idx := -1
	for i, h := range c.Hubs {
		if sameHub(h, in) {
			idx = i
			break
		}
	}
	if idx < 0 {
		for _, h := range c.Hubs {
			if h.ID == in.ID {
				return res, fmt.Errorf("this machine already has a hub %q at %s (CA %s), not the shared one at %s: import it under another id (--id)", h.ID, h.Address, short(NormalizeFingerprint(h.CAFingerprint)), in.Address)
			}
		}
	} else {
		old := c.Hubs[idx]
		in.ID = old.ID // keep the local id; targets follow it
		in.ConfigDir, in.CreatedAt = old.ConfigDir, old.CreatedAt
		if in.CAFingerprint == "" {
			in.CAFingerprint = old.CAFingerprint
		}
		res.HubID, res.Hub = old.ID, "updated"
		if hubEqual(old, in) {
			res.Hub = "unchanged"
		}
	}
	c.UpsertHub(in)
	res.Default = c.DefaultHub == in.ID
	for _, t := range p.Targets {
		t.Hub = in.ID
		old, found := targetByID(c, t.ID)
		c.UpsertTarget(t)
		switch {
		case !found:
			res.TargetsAdded = append(res.TargetsAdded, t.ID)
		case !targetEqual(old, t):
			res.TargetsUpdated = append(res.TargetsUpdated, t.ID)
		}
	}
	return res, nil
}

func sameHub(a, b config.Hub) bool {
	if !strings.EqualFold(a.Address, b.Address) {
		return false
	}
	fa, fb := NormalizeFingerprint(a.CAFingerprint), NormalizeFingerprint(b.CAFingerprint)
	return fa == "" || fb == "" || fa == fb
}

func hubEqual(a, b config.Hub) bool {
	ea, _ := toml.Marshal(a)
	eb, _ := toml.Marshal(b)
	return bytes.Equal(ea, eb)
}

func targetByID(c *config.Config, id string) (config.Target, bool) {
	for _, t := range c.Targets {
		if t.ID == id {
			return t, true
		}
	}
	return config.Target{}, false
}

func targetEqual(old, t config.Target) bool {
	t.AddedAt = old.AddedAt
	if t.ClientName == "" {
		t.ClientName = old.ClientName
	}
	if t.DotfilesBundle == "" && t.DotfilesRev == "" && t.DotfilesAppliedAt == "" {
		t.DotfilesBundle, t.DotfilesRev, t.DotfilesAppliedAt = old.DotfilesBundle, old.DotfilesRev, old.DotfilesAppliedAt
	}
	ea, _ := toml.Marshal(old)
	eb, _ := toml.Marshal(t)
	return bytes.Equal(ea, eb)
}

var pubKeyRE = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com) ([A-Za-z0-9+/]+={0,3})( [^\r\n]*)?$`)

// PublicKey is one OpenSSH public key line.
type PublicKey struct {
	Line        string // as found, trimmed
	Type        string
	Comment     string
	Fingerprint string // SHA256:... as ssh-keygen -l prints it
}

// ParsePublicKey checks one authorized_keys-style line (no options).
func ParsePublicKey(line string) (PublicKey, error) {
	line = strings.TrimSpace(line)
	m := pubKeyRE.FindStringSubmatch(line)
	if m == nil {
		return PublicKey{}, errors.New("not an OpenSSH public key line")
	}
	blob, err := base64.StdEncoding.DecodeString(m[3])
	if err != nil {
		return PublicKey{}, fmt.Errorf("public key: %w", err)
	}
	sum := sha256.Sum256(blob)
	return PublicKey{Line: line, Type: m[1], Comment: strings.TrimSpace(m[4]),
		Fingerprint: "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])}, nil
}

// KnownHostsLines keeps the key lines of `ssh-keygen -F` output (drops its
// "# Host … found" comments and anything that is not a key line).
func KnownHostsLines(out []byte) []string {
	var lines []string
	seen := map[string]bool{}
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") || strings.ContainsAny(l, "\r\x00") || len(strings.Fields(l)) < 3 {
			continue
		}
		if !seen[l] {
			seen[l] = true
			lines = append(lines, l)
		}
	}
	return lines
}

// KnownHostKeyFingerprints are the SHA256 fingerprints of known_hosts lines
// ("ED25519 SHA256:…"), for the review.
func KnownHostKeyFingerprints(lines []string) []string {
	var out []string
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		// [@marker] hosts type key [comment]
		i := 1
		if strings.HasPrefix(f[0], "@") {
			i = 2
		}
		if len(f) < i+2 {
			continue
		}
		blob, err := base64.StdEncoding.DecodeString(f[i+1])
		if err != nil {
			continue
		}
		sum := sha256.Sum256(blob)
		out = append(out, keyLabel(f[i])+" SHA256:"+base64.RawStdEncoding.EncodeToString(sum[:]))
	}
	return out
}

func keyLabel(t string) string {
	switch {
	case t == "ssh-ed25519":
		return "ED25519"
	case t == "ssh-rsa":
		return "RSA"
	case strings.HasPrefix(t, "ecdsa-"):
		return "ECDSA"
	}
	return t
}

// Derive describes a hub found from this machine's Longarm client (`hub
// import` without --stdin).
type Derive struct {
	ID, Name, Mode, Panel, SSHHost, Version, ConfigDir string
}

// DeriveHub builds the hub entry for a client joined as m. Empty fields get
// defaults: the id from the address's first DNS label, private mode for a
// tailnet or private IP, the default loopback panel.
func DeriveHub(m Membership, d Derive) (config.Hub, error) {
	if !m.Joined() {
		return config.Hub{}, errors.New("this machine's longarm is not joined to a hub (no hub.toml address or tls/ca.crt): join it first (`longarm login <hub> --ca-fingerprint <sha256>`), or run `lazylongarm hub share <this host>` on a machine that operates the hub")
	}
	host := m.Address
	if h, _, err := net.SplitHostPort(m.Address); err == nil {
		host = h
	}
	h := config.Hub{ID: d.ID, Name: d.Name, Mode: d.Mode, Address: m.Address, CAFingerprint: m.CAFingerprint,
		SSHHost: d.SSHHost, Panel: d.Panel, Version: d.Version, ConfigDir: d.ConfigDir}
	if h.ID == "" {
		label := host
		if net.ParseIP(host) == nil {
			label, _, _ = strings.Cut(host, ".")
		}
		h.ID = config.Slug(label)
	}
	if h.ID == "" {
		h.ID = "hub"
	}
	if h.Name == "" {
		h.Name = h.ID
	}
	if h.Mode == "" {
		h.Mode = config.ModePublic
		if ip := net.ParseIP(host); ip != nil && (ip.IsPrivate() || tailnet.Contains(ip)) {
			h.Mode = config.ModePrivate
		}
	}
	if h.Panel == "" {
		h.Panel = "127.0.0.1:7480"
	}
	return h, nil
}

var tailnet = func() *net.IPNet { _, n, _ := net.ParseCIDR("100.64.0.0/10"); return n }()
