// Package hubhost renders longarm-hub configuration and parses the files the
// hub writes, for a Debian/Ubuntu host using the official apt package
// (binary /usr/bin/longarm-hub, unit longarm-hub.service, user longarm-hub).
package hubhost

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// Fixed paths of the official package.
const (
	Binary      = "/usr/bin/longarm-hub"
	ConfigPath  = "/etc/longarm-hub/hub.toml"
	DataDir     = "/var/lib/longarm-hub"
	Bootstrap   = DataDir + "/bootstrap.env"
	ServiceName = "longarm-hub"
	ServiceUser = "longarm-hub"

	AptKeyURL         = "https://lubobill1990.github.io/longarm/longarm.gpg"
	AptRepoURL        = "https://lubobill1990.github.io/longarm/deb"
	AptKeyFingerprint = "CA72074CC1D3D23F07704BF9C9B6D4E833105307"
	AptKeyring        = "/etc/apt/keyrings/longarm.gpg"
	AptSources        = "/etc/apt/sources.list.d/longarm.sources"

	// HTTPSDropIn lets the service user bind :443/:80 for the built-in
	// HTTPS panel (the packaged unit runs as longarm-hub without the
	// capability).
	HTTPSDropIn        = "/etc/systemd/system/longarm-hub.service.d/lazylongarm-https.conf"
	HTTPSDropInContent = "# written by `lazylongarm hub panel-access public`: lets longarm-hub bind :443 (and :80)\n[Service]\nAmbientCapabilities=CAP_NET_BIND_SERVICE\n"
)

// Settings are the hub.toml values lazylongarm manages.
type Settings struct {
	Name        string   // certificate name, default "hub"
	Port        int      // gRPC + relay, default 7443
	Stun2       int      // second STUN port, default Port+1
	Panel       string   // http listen, default 127.0.0.1:7480
	PublicAddr  string   // host:port nodes dial
	Hosts       []string // extra certificate SANs
	AutoApprove bool
	RelayRate   string // per relay session, e.g. "32M"

	// Built-in HTTPS panel (`hub panel-access public`); empty = loopback only.
	PublicURL     string // https://<domain>: certificate name, install-script and OAuth base URL
	HTTPS         string // e.g. ":443"
	HTTPSRedirect string // e.g. ":80"
	ACMEEmail     string
}

// Defaults fills unset fields.
func (s Settings) Defaults() Settings {
	if s.Name == "" {
		s.Name = "hub"
	}
	if s.Port == 0 {
		s.Port = 7443
	}
	if s.Stun2 == 0 {
		s.Stun2 = s.Port + 1
	}
	if s.Panel == "" {
		s.Panel = "127.0.0.1:7480"
	}
	if s.RelayRate == "" {
		s.RelayRate = "32M"
	}
	return s
}

// Render returns hub.toml content.
func Render(s Settings) string {
	s = s.Defaults()
	var b strings.Builder
	b.WriteString(ManagedHeader + "\n")
	b.WriteString("# Edit, then `sudo systemctl restart longarm-hub`. All settings: hub.toml.example\n")
	b.WriteString("# next to this file. Re-running hub setup backs it up (hub.toml.bak-*) first.\n")
	fmt.Fprintf(&b, "data_dir = %q\n", DataDir)
	fmt.Fprintf(&b, "name = %q\n", s.Name)
	fmt.Fprintf(&b, "listen = %q                 # gRPC (mTLS) + relay TLS fallback\n", ":"+strconv.Itoa(s.Port))
	fmt.Fprintf(&b, "relay_udp = %q              # relay (QUIC) + STUN\n", ":"+strconv.Itoa(s.Port))
	fmt.Fprintf(&b, "relay_stun2 = %q            # second STUN port\n", ":"+strconv.Itoa(s.Stun2))
	fmt.Fprintf(&b, "relay_rate = %q               # per relay session, bytes/s\n", s.RelayRate)
	if s.HTTPS == "" {
		fmt.Fprintf(&b, "http = %q        # panel: loopback only, reach it with `lazylongarm hub panel`\n", s.Panel)
	} else {
		fmt.Fprintf(&b, "http = %q        # panel over plain http: loopback only (SSH tunnel)\n", s.Panel)
		fmt.Fprintf(&b, "public_url = %q\n", s.PublicURL)
		fmt.Fprintf(&b, "https = %q                  # panel over HTTPS on the Internet (Let's Encrypt for public_url's host)\n", s.HTTPS)
		if s.HTTPSRedirect != "" {
			fmt.Fprintf(&b, "https_redirect = %q          # plain http -> https\n", s.HTTPSRedirect)
		}
		if s.ACMEEmail != "" {
			fmt.Fprintf(&b, "acme_email = %q\n", s.ACMEEmail)
		}
	}
	fmt.Fprintf(&b, "public_addr = %q\n", s.PublicAddr)
	quoted := make([]string, 0, len(s.Hosts))
	for _, h := range s.Hosts {
		quoted = append(quoted, strconv.Quote(h))
	}
	fmt.Fprintf(&b, "hosts = [%s]\n", strings.Join(quoted, ", "))
	fmt.Fprintf(&b, "auto_approve = %v\n", s.AutoApprove)
	b.WriteString("\n[certs]\nworker_ttl = \"2160h\"\nclient_ttl = \"720h\"\n")
	b.WriteString("\n# No embedded Tailscale leg: nodes reach this hub over TCP/QUIC directly.\n[tailnet]\nprovider = \"none\"\n")
	return b.String()
}

// ManagedHeader starts every hub.toml lazylongarm writes.
const ManagedHeader = "# longarm-hub configuration, written by `lazylongarm hub setup`."

// ParseSettings reads the keys lazylongarm manages back from hub.toml, so a
// later rewrite (e.g. `hub panel-access`) keeps the port, relay rate and
// approval choices. handEdited is true when re-rendering would change more
// than these keys (the rewrite would drop unknown keys or comments).
func ParseSettings(content string) (s Settings, handEdited bool, err error) {
	var raw struct {
		Name          string   `toml:"name"`
		Listen        string   `toml:"listen"`
		RelayStun2    string   `toml:"relay_stun2"`
		RelayRate     string   `toml:"relay_rate"`
		HTTP          string   `toml:"http"`
		PublicURL     string   `toml:"public_url"`
		HTTPS         string   `toml:"https"`
		HTTPSRedirect string   `toml:"https_redirect"`
		ACMEEmail     string   `toml:"acme_email"`
		PublicAddr    string   `toml:"public_addr"`
		Hosts         []string `toml:"hosts"`
		AutoApprove   bool     `toml:"auto_approve"`
	}
	if err := toml.Unmarshal([]byte(content), &raw); err != nil {
		return s, false, fmt.Errorf("parse hub.toml: %w", err)
	}
	port := func(addr string) int {
		if i := strings.LastIndexByte(addr, ':'); i >= 0 {
			n, _ := strconv.Atoi(addr[i+1:])
			return n
		}
		return 0
	}
	s = Settings{Name: raw.Name, Port: port(raw.Listen), Stun2: port(raw.RelayStun2), Panel: raw.HTTP, PublicAddr: raw.PublicAddr,
		Hosts: raw.Hosts, AutoApprove: raw.AutoApprove, RelayRate: raw.RelayRate,
		PublicURL: raw.PublicURL, HTTPS: raw.HTTPS, HTTPSRedirect: raw.HTTPSRedirect, ACMEEmail: raw.ACMEEmail}.Defaults()
	if s.PublicAddr == "" {
		return s, false, errors.New("hub.toml has no public_addr")
	}
	handEdited = !strings.HasPrefix(content, ManagedHeader) || Render(s) != content
	return s, handEdited, nil
}

// SHA256 of content (hex).
func SHA256(content string) string {
	s := sha256.Sum256([]byte(content))
	return hex.EncodeToString(s[:])
}

// BootstrapState is the non-secret part of bootstrap.env plus the code.
type BootstrapState struct {
	Fingerprint string
	HubAddress  string
	AdminJoined bool
	AdminCode   string // secret: callers wrap it immediately
	CodeExpires time.Time
}

// ParseBootstrap parses bootstrap.env key=value lines.
func ParseBootstrap(text string) BootstrapState {
	var st BootstrapState
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "ca_fingerprint":
			st.Fingerprint = v
		case "hub_address":
			st.HubAddress = v
		case "admin_joined":
			st.AdminJoined = v == "true"
		case "admin_join_code":
			st.AdminCode = v
		case "admin_join_code_expires":
			st.CodeExpires, _ = time.Parse(time.RFC3339, v)
		}
	}
	return st
}

// CodeUsable reports whether the bootstrap admin code can still be used.
func (b BootstrapState) CodeUsable(now time.Time) bool {
	return !b.AdminJoined && b.AdminCode != "" && b.CodeExpires.After(now.Add(time.Minute))
}

// HealthOK reports whether a /healthz body says "ok": true (the hub
// pretty-prints its JSON).
func HealthOK(body string) bool {
	var h struct {
		OK bool `json:"ok"`
	}
	return json.Unmarshal([]byte(body), &h) == nil && h.OK
}

// HealthSummary is a one-line digest of a /healthz body.
func HealthSummary(body string) string {
	var h map[string]any
	if json.Unmarshal([]byte(body), &h) != nil {
		s := strings.Join(strings.Fields(body), " ")
		if len(s) > 120 {
			s = s[:120] + "…"
		}
		return s
	}
	parts := []string{fmt.Sprintf("ok=%v", h["ok"])}
	for _, k := range []string{"version", "online", "uptime_seconds"} {
		if v, ok := h[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	return strings.Join(parts, " ")
}

// ParseFingerprint extracts the value from `longarm-hub ca show` output.
func ParseFingerprint(out string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "fingerprint" {
			return fields[1]
		}
	}
	return ""
}

// AptSourcesContent is the deb822 source for the official repository.
func AptSourcesContent() string {
	return "Types: deb\nURIs: " + AptRepoURL + "\nSuites: stable\nComponents: main\nArchitectures: amd64 arm64\nSigned-By: " + AptKeyring + "\n"
}

// Heredoc returns a here-document body with a delimiter absent from content.
func Heredoc(content string) (string, string) {
	delim := "LAZYLONGARM_EOF"
	for i := 0; strings.Contains(content, delim); i++ {
		delim = fmt.Sprintf("LAZYLONGARM_EOF_%d", i)
	}
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return content, delim
}
