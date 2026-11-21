// Package tailscalehost holds what lazylongarm needs to put a hub's web
// panel on a tailnet: the official apt repository (signing key pinned) and
// parsers for `tailscale status --json` / `tailscale serve status --json`.
package tailscalehost

import (
	"encoding/json"
	"strings"
)

// Official apt repository (https://pkgs.tailscale.com/stable/).
const (
	PkgsURL        = "https://pkgs.tailscale.com/stable"
	KeyFingerprint = "2596A99EAAB33821893C0A79458CA832957F5868"
	Keyring        = "/usr/share/keyrings/tailscale-archive-keyring.gpg"
	Sources        = "/etc/apt/sources.list.d/tailscale.list"
	// AdminDNS is where MagicDNS and HTTPS certificates are switched on.
	AdminDNS = "https://login.tailscale.com/admin/dns"
)

// KeyURL is the repository key for a distro ("ubuntu", "debian") release.
func KeyURL(distro, codename string) string {
	return PkgsURL + "/" + distro + "/" + codename + ".noarmor.gpg"
}

// SourcesContent is the apt source line for a distro release.
func SourcesContent(distro, codename string) string {
	return "# Tailscale packages for " + distro + " " + codename + " (written by lazylongarm)\n" +
		"deb [signed-by=" + Keyring + "] " + PkgsURL + "/" + distro + " " + codename + " main\n"
}

// Status is the part of `tailscale status --json` lazylongarm reads.
type Status struct {
	BackendState   string `json:"BackendState"`   // Running | NeedsLogin | Stopped | ...
	MagicDNSSuffix string `json:"MagicDNSSuffix"` // "tailnet-1234.ts.net"
	Self           struct {
		DNSName      string   `json:"DNSName"` // "hub.tailnet-1234.ts.net."
		TailscaleIPs []string `json:"TailscaleIPs"`
	} `json:"Self"`
	CertDomains []string `json:"CertDomains"` // empty when HTTPS certificates are off
}

// ParseStatus decodes `tailscale status --json`.
func ParseStatus(data []byte) (Status, error) {
	var s Status
	err := json.Unmarshal(data, &s)
	return s, err
}

// Running reports whether the node is logged in and up.
func (s Status) Running() bool { return s.BackendState == "Running" }

// DNSName is the node's MagicDNS name without the trailing dot.
func (s Status) DNSName() string { return strings.TrimSuffix(s.Self.DNSName, ".") }

// IPv4 is the node's tailnet IPv4 address ("" when none).
func (s Status) IPv4() string {
	for _, ip := range s.Self.TailscaleIPs {
		if !strings.Contains(ip, ":") {
			return ip
		}
	}
	return ""
}

// HTTPSEnabled reports whether the tailnet issues certificates for this node.
func (s Status) HTTPSEnabled() bool { return len(s.CertDomains) > 0 }

// URL is the panel URL tailscale serve gives this node.
func (s Status) URL() string {
	if s.DNSName() == "" {
		return ""
	}
	return "https://" + s.DNSName() + "/"
}

// Serves reports whether `tailscale serve status --json` output proxies
// HTTPS port 443, path "/", to target (e.g. "http://127.0.0.1:7480").
func Serves(data []byte, target string) bool {
	var st struct {
		Web map[string]struct {
			Handlers map[string]struct {
				Proxy string `json:"Proxy"`
			} `json:"Handlers"`
		} `json:"Web"`
	}
	if json.Unmarshal(data, &st) != nil {
		return false
	}
	for hostPort, web := range st.Web {
		if !strings.HasSuffix(hostPort, ":443") {
			continue
		}
		if h, ok := web.Handlers["/"]; ok && strings.TrimSuffix(h.Proxy, "/") == strings.TrimSuffix(target, "/") {
			return true
		}
	}
	return false
}

// ServesHTTPS reports whether `tailscale serve status --json` output has
// anything on HTTPS port 443 (whatever it proxies to).
func ServesHTTPS(data []byte) bool {
	var st struct {
		TCP map[string]json.RawMessage `json:"TCP"`
		Web map[string]json.RawMessage `json:"Web"`
	}
	if json.Unmarshal(data, &st) != nil {
		return false
	}
	if _, ok := st.TCP["443"]; ok {
		return true
	}
	for hostPort := range st.Web {
		if strings.HasSuffix(hostPort, ":443") {
			return true
		}
	}
	return false
}
