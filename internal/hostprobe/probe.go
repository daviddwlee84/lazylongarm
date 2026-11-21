// Package hostprobe collects read-only facts about a machine (local or over
// SSH) with a POSIX sh script. It assumes no Python (macOS's /usr/bin/python3
// may be an installer stub) and never prompts (sudo is tried with -n only).
package hostprobe

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/connection"
)

//go:embed probe.sh
var script string

// Facts about one machine.
type Facts struct {
	Host          string            `json:"host"` // SSH alias, "" = local
	OS            string            `json:"os"`   // linux | darwin
	Arch          string            `json:"arch"` // amd64 | arm64 (Go names)
	RawArch       string            `json:"raw_arch,omitempty"`
	Distro        string            `json:"distro,omitempty"`
	DistroID      string            `json:"distro_id,omitempty"`
	DistroCode    string            `json:"distro_codename,omitempty"` // VERSION_CODENAME, e.g. jammy
	User          string            `json:"user"`
	UID           int               `json:"uid"`
	Groups        []string          `json:"groups,omitempty"` // the user's Unix groups (id -Gn)
	Home          string            `json:"home"`
	Hostname      string            `json:"hostname,omitempty"`
	Sudo          string            `json:"sudo"` // nopasswd | password | absent
	Has           map[string]bool   `json:"has"`
	TailscaleIP   string            `json:"tailscale_ip,omitempty"`
	Bins          map[string]string `json:"bins,omitempty"`     // longarm/longarmd/longarm-hub → path
	Versions      map[string]string `json:"versions,omitempty"` // → version string
	WorkerEnroll  bool              `json:"worker_enrolled,omitempty"`
	WorkerHub     []string          `json:"worker_hub,omitempty"`
	ClientEnroll  bool              `json:"client_enrolled,omitempty"`
	Services      map[string]string `json:"services,omitempty"` // longarmd, longarmd_user, longarm_hub → systemd state
	Packages      map[string]string `json:"packages,omitempty"`
	AptRepo       bool              `json:"apt_repo,omitempty"`
	MemTotalKB    int64             `json:"mem_total_kb,omitempty"`
	MemAvailKB    int64             `json:"mem_avail_kb,omitempty"`
	CPUs          int               `json:"cpus,omitempty"`
	Listening     []string          `json:"listening,omitempty"` // "tcp 0.0.0.0:443"
	Containers    []string          `json:"containers,omitempty"`
	Clock         int64             `json:"clock,omitempty"`      // the machine's Unix time when probed
	ClockSkew     time.Duration     `json:"clock_skew,omitempty"` // its clock minus this machine's (± half the probe time)
	ProbedAt      time.Time         `json:"probed_at"`
	ProbeDuration time.Duration     `json:"probe_duration"`
}

// Probe runs the script on host ("" = this machine).
func Probe(ctx context.Context, host string) (*Facts, error) {
	start := time.Now()
	res, err := connection.Run(ctx, connection.Request{Host: host, Script: script, Timeout: 45 * time.Second, Limit: 1 << 20})
	if err != nil {
		var cmdErr *connection.CommandError
		if !errors.As(err, &cmdErr) || len(res.Stdout) == 0 {
			return nil, err
		}
	}
	f, err := Parse(string(res.Stdout))
	if err != nil {
		return nil, err
	}
	f.Host = host
	f.ProbedAt = time.Now()
	f.ProbeDuration = time.Since(start)
	if f.Clock > 0 {
		mid := start.Add(f.ProbeDuration / 2)
		f.ClockSkew = time.Unix(f.Clock, 0).Sub(mid).Truncate(time.Second)
	}
	return f, nil
}

// Parse decodes probe output.
func Parse(out string) (*Facts, error) {
	f := &Facts{Has: map[string]bool{}, Bins: map[string]string{}, Versions: map[string]string{}, Services: map[string]string{}, Packages: map[string]string{}}
	done := false
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch {
		case k == "os":
			f.OS = strings.ToLower(v)
		case k == "arch":
			f.RawArch = v
			f.Arch = goArch(v)
		case k == "distro_pretty":
			f.Distro = v
		case k == "distro_id":
			f.DistroID = v
		case k == "distro_codename":
			f.DistroCode = v
		case k == "user":
			f.User = v
		case k == "uid":
			f.UID, _ = strconv.Atoi(v)
		case k == "groups":
			f.Groups = strings.Fields(v)
		case k == "home":
			f.Home = v
		case k == "hostname":
			f.Hostname = v
		case k == "sudo":
			f.Sudo = v
		case strings.HasPrefix(k, "has_"):
			f.Has[strings.TrimPrefix(k, "has_")] = v == "yes"
		case k == "tailscale_ip":
			f.TailscaleIP = v
		case strings.HasPrefix(k, "bin_"):
			f.Bins[strings.TrimPrefix(k, "bin_")] = v
		case strings.HasPrefix(k, "ver_"):
			f.Versions[strings.TrimPrefix(k, "ver_")] = ExtractVersion(v)
		case k == "worker_enrolled":
			f.WorkerEnroll = true
		case k == "worker_hub":
			f.WorkerHub = append(f.WorkerHub, v)
		case k == "client_enrolled":
			f.ClientEnroll = true
		case strings.HasPrefix(k, "svc_"):
			if v != "" {
				f.Services[strings.TrimPrefix(k, "svc_")] = v
			}
		case strings.HasPrefix(k, "pkg_"):
			f.Packages[strings.TrimPrefix(k, "pkg_")] = v
		case k == "apt_repo":
			f.AptRepo = true
		case k == "mem_total_kb":
			f.MemTotalKB, _ = strconv.ParseInt(v, 10, 64)
		case k == "mem_avail_kb":
			f.MemAvailKB, _ = strconv.ParseInt(v, 10, 64)
		case k == "cpus":
			f.CPUs, _ = strconv.Atoi(v)
		case k == "listen":
			f.Listening = append(f.Listening, v)
		case k == "container":
			f.Containers = append(f.Containers, v)
		case k == "clock":
			f.Clock, _ = strconv.ParseInt(v, 10, 64)
		case k == "probe_done":
			done = true
		}
	}
	if !done || f.OS == "" {
		return nil, errors.New("host probe did not complete; is the remote login shell POSIX sh compatible?")
	}
	sort.Strings(f.Listening)
	return f, nil
}

var versionRE = regexp.MustCompile(`v?\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.\-+]+)?`)

// ExtractVersion finds the first semver-looking token.
func ExtractVersion(s string) string {
	m := versionRE.FindString(s)
	if m == "" {
		return strings.TrimSpace(s)
	}
	if !strings.HasPrefix(m, "v") {
		m = "v" + m
	}
	return m
}

func goArch(raw string) string {
	switch strings.ToLower(raw) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		return strings.ToLower(raw)
	}
}

// Ports returns the set of "proto/port" pairs that are listening.
func (f *Facts) Ports() map[string]string {
	out := map[string]string{}
	for _, l := range f.Listening {
		proto, addr, ok := strings.Cut(l, " ")
		if !ok {
			continue
		}
		proto = strings.TrimSuffix(strings.TrimSuffix(proto, "6"), "4")
		i := strings.LastIndexAny(addr, ":.")
		if i < 0 {
			continue
		}
		port := addr[i+1:]
		if _, err := strconv.Atoi(port); err != nil {
			continue
		}
		out[proto+"/"+port] = addr
	}
	return out
}

// PortBusy reports whether proto ("tcp"/"udp") port is listening.
func (f *Facts) PortBusy(proto string, port int) bool {
	_, ok := f.Ports()[fmt.Sprintf("%s/%d", proto, port)]
	return ok
}

// Platform is "<os>/<arch>".
func (f *Facts) Platform() string { return f.OS + "/" + f.Arch }

// Where names the machine for messages.
func (f *Facts) Where() string {
	if f.Host == "" {
		return "this machine"
	}
	return f.Host
}

// MemSummary is a short human summary.
func (f *Facts) MemSummary() string {
	if f.MemTotalKB == 0 {
		return "unknown"
	}
	s := fmt.Sprintf("%.1f GB total", float64(f.MemTotalKB)/1024/1024)
	if f.MemAvailKB > 0 {
		s += fmt.Sprintf(", %.1f GB available", float64(f.MemAvailKB)/1024/1024)
	}
	return s
}

// MaxClockSkew is how far a machine's clock may be off before a plan warns.
// Proxies such as VMess reject clients more than ~2 minutes off, and TLS
// certificates look not-yet-valid or expired to a clock that is far off.
const MaxClockSkew = 2 * time.Minute

// ClockWarning describes a clock that is off by more than MaxClockSkew ("" when fine
// or unknown).
func (f *Facts) ClockWarning() string {
	d := f.ClockSkew
	if f.Clock == 0 || (d <= MaxClockSkew && d >= -MaxClockSkew) {
		return ""
	}
	dir := "ahead of"
	if d < 0 {
		dir, d = "behind", -d
	}
	return fmt.Sprintf("the clock on %s is %s %s this machine's: TLS to the hub and proxies such as VMess can fail until it is synced (macOS: Date & Time → Set automatically; Linux: timedatectl set-ntp true)", f.Where(), strings.TrimSuffix(d.Round(time.Minute).String(), "0s"), dir)
}

// RootEquivalentGroups are groups whose members can become root without a
// password (docker/lxd run privileged containers, libvirt starts VMs with host
// devices, disk reads raw block devices).
var RootEquivalentGroups = []string{"docker", "lxd", "libvirt", "disk"}

// RootEquivalent lists why the probed account can act as root without a
// password: root-equivalent groups and passwordless sudo.
func (f *Facts) RootEquivalent() []string {
	var why []string
	for _, g := range f.Groups {
		for _, r := range RootEquivalentGroups {
			if g == r {
				why = append(why, "group "+g)
			}
		}
	}
	if f.Sudo == "nopasswd" {
		why = append(why, "passwordless sudo")
	}
	return why
}
