package hubshare

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/config"
)

func testCA(t *testing.T) ([]byte, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), hex.EncodeToString(sum[:])
}

func TestCAFingerprintAndMembership(t *testing.T) {
	caPEM, want := testCA(t)
	got, err := CAFingerprint(caPEM)
	if err != nil || got != want {
		t.Fatalf("fingerprint %q %v, want %q", got, err, want)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tls"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "tls", "ca.crt"), caPEM, 0o600)
	_ = os.WriteFile(filepath.Join(dir, "hub.toml"), []byte("# written by join\naddress = \"hub.example.com:7443\"\n"), 0o600)
	m, err := LocalMembership(dir)
	if err != nil || !m.Joined() || m.Address != "hub.example.com:7443" || m.CAFingerprint != want {
		t.Fatalf("membership %+v %v", m, err)
	}
	h := config.Hub{ID: "h", Address: "hub.example.com:7443", CAFingerprint: "sha256:" + strings.ToUpper(want)}
	if err := m.Matches(h); err != nil {
		t.Fatalf("should match: %v", err)
	}
	h.CAFingerprint = "sha256:" + strings.Repeat("0", 64)
	if err := m.Matches(h); err == nil || !strings.Contains(err.Error(), "another hub") {
		t.Fatalf("other CA must not match: %v", err)
	}
	if err := (Membership{}).Matches(h); err == nil {
		t.Fatal("unjoined must not match")
	}
	empty, err := LocalMembership(t.TempDir())
	if err != nil || empty.Joined() {
		t.Fatalf("empty dir: %+v %v", empty, err)
	}
}

func TestPayloadRoundTripDropsLocalFields(t *testing.T) {
	h := config.Hub{ID: "h", Mode: "public", Address: "hub.example.com:7443", CAFingerprint: "sha256:AB", SSHHost: "hub-box",
		ConfigDir: "/home/me/.lc", CreatedAt: "2026-01-01T00:00:00Z", PanelAccess: &config.PanelAccess{Mode: "tailnet", URL: "https://hub.example.ts.net/"}}
	p := NewPayload(h, []config.Target{{ID: "w1", SSHHost: "w1", Hub: "h", AddedAt: "x"}}, "me")
	b, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "/home/me") || strings.Contains(string(b), "2026-01-01") {
		t.Fatalf("machine-local fields leaked:\n%s", b)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hub.CAFingerprint != "ab" || got.Hub.PanelAccess == nil || got.Hub.PanelAccess.URL != h.PanelAccess.URL || len(got.Targets) != 1 || got.From != "me" {
		t.Fatalf("round trip: %+v", got)
	}
	if _, err := Decode([]byte("version = 99\n[hub]\nid='h'\naddress='a'\n")); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("newer version: %v", err)
	}
	if _, err := Decode([]byte("x = 1")); err == nil {
		t.Fatal("not a payload")
	}
}

func TestMerge(t *testing.T) {
	c := &config.Config{}
	in := Payload{Version: 1, Hub: config.Hub{ID: "h", Mode: "public", Address: "hub.example.com:7443", CAFingerprint: "aa", SSHHost: "hub-box"},
		Targets: []config.Target{{ID: "w1", SSHHost: "w1"}}}
	r, err := Merge(c, in)
	if err != nil || r.Hub != "added" || !r.Default || c.DefaultHub != "h" || len(r.TargetsAdded) != 1 || c.Targets[0].Hub != "h" {
		t.Fatalf("add: %+v %v %+v", r, err, c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	// Same hub under a local id with a local config dir: updated in place.
	c.Hubs[0].ID, c.DefaultHub, c.Targets[0].Hub = "mine", "mine", "mine"
	c.Hubs[0].ConfigDir = "/local"
	r, err = Merge(c, in)
	if err != nil || r.HubID != "mine" || r.Hub != "unchanged" || c.Hubs[0].ConfigDir != "/local" || len(c.Hubs) != 1 || len(r.TargetsAdded)+len(r.TargetsUpdated) != 0 {
		t.Fatalf("again: %+v %v %+v", r, err, c.Hubs)
	}
	in.Hub.SSHHost = "other-alias"
	if r, _ = Merge(c, in); r.Hub != "updated" || c.Hubs[0].SSHHost != "other-alias" {
		t.Fatalf("update: %+v", r)
	}
	// Another hub under an id that is taken: refused.
	c2 := &config.Config{Hubs: []config.Hub{{ID: "h", Mode: "public", Address: "other.example.com:7443", CAFingerprint: "bb"}}}
	if _, err := Merge(c2, Payload{Version: 1, Hub: config.Hub{ID: "h", Mode: "public", Address: "hub.example.com:7443", CAFingerprint: "aa"}}); err == nil || !strings.Contains(err.Error(), "--id") {
		t.Fatalf("id clash: %v", err)
	}
}

func TestPublicKeyAndKnownHosts(t *testing.T) {
	k, err := ParsePublicKey("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl me@laptop\n")
	if err != nil || k.Type != "ssh-ed25519" || k.Comment != "me@laptop" || !strings.HasPrefix(k.Fingerprint, "SHA256:") {
		t.Fatalf("%+v %v", k, err)
	}
	for _, bad := range []string{"", "ssh-ed25519", "command=\"x\" ssh-ed25519 AAAA", "ssh-ed25519 AAAA\nssh-rsa BBBB"} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	out := []byte("# Host hub.example.com found: line 3\nhub.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n|1|salt|hash ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\nhub.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n")
	lines := KnownHostsLines(out)
	if len(lines) != 2 {
		t.Fatalf("lines %q", lines)
	}
	fps := KnownHostKeyFingerprints(lines)
	if len(fps) != 2 || fps[0] != "ED25519 "+k.Fingerprint {
		t.Fatalf("fps %q (key %s)", fps, k.Fingerprint)
	}
}

func TestDeriveHub(t *testing.T) {
	m := Membership{Address: "hub.example.com:7443", CAFingerprint: "aa"}
	h, err := DeriveHub(m, Derive{SSHHost: "hub-box"})
	if err != nil || h.ID != "hub" || h.Mode != config.ModePublic || h.Panel != "127.0.0.1:7480" || h.SSHHost != "hub-box" {
		t.Fatalf("%+v %v", h, err)
	}
	h, _ = DeriveHub(Membership{Address: "100.101.102.103:7443", CAFingerprint: "aa"}, Derive{})
	if h.Mode != config.ModePrivate || h.ID != "100-101-102-103" {
		t.Fatalf("tailnet: %+v", h)
	}
	if _, err := DeriveHub(Membership{}, Derive{}); err == nil {
		t.Fatal("unjoined")
	}
}
