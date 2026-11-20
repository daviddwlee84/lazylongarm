package longarm

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Int64 accepts protojson's int64-as-string as well as JSON numbers.
type Int64 int64

func (n *Int64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return err
		}
		v = int64(f)
	}
	*n = Int64(v)
	return nil
}

// VersionInfo is `longarm version --json`.
type VersionInfo struct {
	Version    string   `json:"version"`
	Features   []string `json:"features"`
	DefaultHub string   `json:"default_hub,omitempty"`
}

// WhoAmI is `longarm whoami --json` (snake_case).
type WhoAmI struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	User         string `json:"user"`
	Role         string `json:"role"`
	HubName      string `json:"hub_name"`
	CertNotAfter string `json:"cert_not_after"`
	Tenancy      bool   `json:"tenancy"`
}

// WorkerList is `longarm worker ls --json` (snake_case).
type WorkerList struct {
	Nodes []Node `json:"nodes"`
}

// Node is one machine in the registry.
type Node struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Machine    string            `json:"machine"`
	Hostname   string            `json:"hostname"`
	OS         string            `json:"os"`
	Arch       string            `json:"arch"`
	Role       string            `json:"role"`
	State      string            `json:"state"`
	Online     bool              `json:"online"`
	Owner      string            `json:"owner"`
	Version    string            `json:"version"`
	Endpoints  []string          `json:"endpoints"`
	Labels     map[string]string `json:"labels"`
	LastSeen   string            `json:"last_seen"`
	JoinedAt   string            `json:"joined_at"`
	JobsActive int               `json:"jobs_active"`
	ReservedGB float64           `json:"reserved_gb"`
	Isolation  string            `json:"isolation"`
	Metrics    *Metrics          `json:"metrics,omitempty"`
}

// Metrics reported by a worker.
type Metrics struct {
	CPUPercent     float64 `json:"cpu_percent"`
	MemTotalBytes  Int64   `json:"mem_total_bytes"`
	MemUsedBytes   Int64   `json:"mem_used_bytes"`
	DiskFreeBytes  Int64   `json:"disk_free_bytes"`
	DiskTotalBytes Int64   `json:"disk_total_bytes"`
	JobsRunning    int     `json:"jobs_running"`
	Threads        int     `json:"threads"`
	UptimeSeconds  Int64   `json:"uptime_seconds"`
	Time           string  `json:"time"`
}

// IsWorker reports whether the node runs jobs (admins/clients are devices).
func (n Node) IsWorker() bool { return n.Role == "" || n.Role == "worker" }

// MemPercent is used/total memory in percent (0 when unknown).
func (n Node) MemPercent() float64 {
	if n.Metrics == nil || n.Metrics.MemTotalBytes == 0 {
		return 0
	}
	return float64(n.Metrics.MemUsedBytes) * 100 / float64(n.Metrics.MemTotalBytes)
}

// Path guesses how this machine is reached from its endpoints.
func (n Node) Path() string {
	var kinds []string
	for _, e := range n.Endpoints {
		scheme, _, _ := strings.Cut(e, "://")
		switch scheme {
		case "longnet":
			kinds = append(kinds, "relay")
		case "tcp":
			if strings.Contains(e, "127.0.0.1") || strings.Contains(e, "[::1]") {
				continue
			}
			kinds = append(kinds, "direct")
		case "tailscale":
			kinds = append(kinds, "tailnet")
		}
	}
	if len(kinds) == 0 {
		return "-"
	}
	return strings.Join(uniq(kinds), "+")
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// JobList is `longarm jobs --json` (camelCase, protojson).
type JobList struct {
	Jobs   []FleetJob `json:"jobs"`
	Source string     `json:"source"`
}

// FleetJob is one job with its worker.
type FleetJob struct {
	Job          Job    `json:"job"`
	UpdatedAt    string `json:"updatedAt"`
	Worker       string `json:"worker"`
	WorkerID     string `json:"workerId"`
	WorkerOnline bool   `json:"workerOnline"`
}

// Job state and spec.
type Job struct {
	CreatedAt   string  `json:"createdAt"`
	StartedAt   string  `json:"startedAt"`
	EndedAt     string  `json:"endedAt"`
	ExitCode    int     `json:"exitCode"`
	HasExitCode bool    `json:"hasExitCode"`
	PID         int     `json:"pid"`
	Reason      string  `json:"reason"`
	State       string  `json:"state"`
	LogSize     Int64   `json:"logSize"`
	Spec        JobSpec `json:"spec"`
}

// JobSpec as submitted.
type JobSpec struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Argv        []string          `json:"argv"`
	Owner       string            `json:"owner"`
	SubmittedBy string            `json:"submittedBy"`
	Labels      map[string]string `json:"labels"`
}

// ShortState turns JOB_STATE_RUNNING into running.
func (j Job) ShortState() string {
	return strings.ToLower(strings.TrimPrefix(j.State, "JOB_STATE_"))
}

// Active reports whether the job may still be running.
func (j Job) Active() bool {
	switch j.ShortState() {
	case "running", "queued", "pending", "starting":
		return true
	}
	return false
}

// Duration since start (or until end).
func (j Job) Duration(now time.Time) time.Duration {
	start, err := time.Parse(time.RFC3339Nano, j.StartedAt)
	if err != nil {
		return 0
	}
	end := now
	if e, err := time.Parse(time.RFC3339Nano, j.EndedAt); err == nil {
		end = e
	}
	return end.Sub(start).Round(time.Second)
}

// Doctor is `longarm doctor --json` (schema 1).
type Doctor struct {
	OK      bool          `json:"ok"`
	Schema  int           `json:"schema"`
	Tool    string        `json:"tool"`
	Version string        `json:"version"`
	Checks  []DoctorCheck `json:"checks"`
}

// DoctorCheck is one diagnosis line.
type DoctorCheck struct {
	ID     string          `json:"id"`
	Title  string          `json:"title"`
	Status string          `json:"status"` // ok | warn | fail
	Detail string          `json:"detail"`
	Fix    string          `json:"fix"`
	Data   json.RawMessage `json:"data,omitempty"`
}
