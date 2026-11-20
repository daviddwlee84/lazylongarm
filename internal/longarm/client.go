// Package longarm adapts the Longarm CLI's stable programmatic contract
// (docs/programs.md: exit codes, `--json` output, feature names). It never
// talks to the client daemon's internal HTTP API or reads Longarm's files.
package longarm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/secret"
)

var commandContext = exec.CommandContext

// Kind classifies a Longarm CLI failure by its documented exit code.
type Kind string

const (
	KindBadRequest   Kind = "bad-request"     // 2: nothing done
	KindNotFound     Kind = "not-found"       // 3
	KindHubDown      Kind = "hub-unreachable" // 69: --hub-only and the hub did not answer
	KindRefused      Kind = "refused"         // 75: capacity, quota or an --if-* condition
	KindNotAllowed   Kind = "not-allowed"     // 77: permission, or sign-in expired
	KindKilled       Kind = "killed"          // 137
	KindLost         Kind = "lost"            // 254
	KindUnknown      Kind = "unknown"         // 255: anything else; outcome may be unknown
	KindNotInstalled Kind = "not-installed"
)

// Error is a failed longarm invocation.
type Error struct {
	Kind     Kind
	ExitCode int
	Args     string
	Stderr   string
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if len(msg) > 600 {
		msg = msg[len(msg)-600:]
	}
	if msg == "" {
		msg = fmt.Sprintf("exit status %d", e.ExitCode)
	}
	if e.Args == "" {
		return msg
	}
	return fmt.Sprintf("longarm %s: %s (%s)", e.Args, msg, e.Kind)
}

// KindOf returns the Kind of err, or "" when it is not an *Error.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}

func kindFor(code int) Kind {
	switch code {
	case 2:
		return KindBadRequest
	case 3:
		return KindNotFound
	case 69:
		return KindHubDown
	case 75:
		return KindRefused
	case 77:
		return KindNotAllowed
	case 137:
		return KindKilled
	case 254:
		return KindLost
	default:
		return KindUnknown
	}
}

// Client runs one longarm binary against one client config directory.
type Client struct {
	Bin       string // path to longarm; empty = discover
	ConfigDir string // LONGARM_CONFIG_DIR; empty = Longarm's default (~/.config/longarm)

	once     sync.Once
	features map[string]bool
	version  string
	verErr   error
}

// Find locates longarm on PATH or in ~/.local/bin (where lazylongarm installs it).
func Find() (string, error) {
	if p, err := exec.LookPath("longarm"); err == nil {
		return p, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".local", "bin", "longarm")
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", &Error{Kind: KindNotInstalled, Args: "", Stderr: "longarm is not installed on this machine (`lazylongarm hub setup` installs it into ~/.local/bin)"}
}

func (c *Client) bin() (string, error) {
	if c.Bin != "" {
		return c.Bin, nil
	}
	p, err := Find()
	if err != nil {
		return "", err
	}
	c.Bin = p
	return p, nil
}

// Run executes longarm with args and returns stdout.
func (c *Client) Run(ctx context.Context, timeout time.Duration, stdin []byte, args ...string) ([]byte, error) {
	bin, err := c.bin()
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := commandContext(ctx, bin, args...)
	cmd.Env = c.env()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	if err == nil {
		return out.Bytes(), nil
	}
	display := redactArgs(args)
	if ctx.Err() != nil {
		return out.Bytes(), &Error{Kind: KindUnknown, ExitCode: -1, Args: display, Stderr: "timed out after " + timeout.String()}
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out.Bytes(), &Error{Kind: kindFor(exit.ExitCode()), ExitCode: exit.ExitCode(), Args: display, Stderr: errb.String()}
	}
	return nil, fmt.Errorf("run longarm: %w", err)
}

// Command returns an *exec.Cmd for interactive use (attach, logs -f, ssh).
func (c *Client) Command(ctx context.Context, args ...string) (*exec.Cmd, error) {
	bin, err := c.bin()
	if err != nil {
		return nil, err
	}
	cmd := commandContext(ctx, bin, args...)
	cmd.Env = c.env()
	return cmd, nil
}

func (c *Client) env() []string {
	env := append(os.Environ(), "LONGARM_NO_UPDATE_NOTICE=1")
	if c.ConfigDir != "" {
		env = append(env, "LONGARM_CONFIG_DIR="+c.ConfigDir)
	}
	return env
}

func redactArgs(args []string) string {
	out := make([]string, len(args))
	copy(out, args)
	// `join <hub> <code>`: never echo the code.
	for i := 0; i+2 < len(out); i++ {
		if out[i] == "join" {
			out[i+2] = "<code>"
		}
	}
	return strings.Join(out, " ")
}

func (c *Client) json(ctx context.Context, v any, args ...string) error {
	out, err := c.Run(ctx, 30*time.Second, nil, args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("decode `longarm %s` output: %w", strings.Join(args, " "), err)
	}
	return nil
}

// Version returns `longarm version --json` (cached per client).
func (c *Client) Version(ctx context.Context) (VersionInfo, error) {
	var v VersionInfo
	err := c.json(ctx, &v, "version", "--json")
	if err == nil {
		c.once.Do(func() {
			c.version = v.Version
			c.features = map[string]bool{}
			for _, f := range v.Features {
				c.features[f] = true
			}
		})
	}
	return v, err
}

// Has reports whether the installed CLI advertises feature (checked once).
func (c *Client) Has(ctx context.Context, feature string) bool {
	if c.features == nil {
		if _, err := c.Version(ctx); err != nil {
			return false
		}
	}
	return c.features[feature]
}

// WhoAmI returns `longarm whoami --json`.
func (c *Client) WhoAmI(ctx context.Context) (WhoAmI, error) {
	var w WhoAmI
	return w, c.json(ctx, &w, "whoami", "--json")
}

// Workers returns `longarm worker ls --json`.
func (c *Client) Workers(ctx context.Context) (WorkerList, error) {
	var w WorkerList
	return w, c.json(ctx, &w, "worker", "ls", "--json")
}

// Jobs returns `longarm jobs --json` (all jobs the caller may see).
func (c *Client) Jobs(ctx context.Context, limit int) (JobList, error) {
	var j JobList
	args := []string{"jobs", "--json", "--all"}
	if limit > 0 {
		args = append(args, "--limit", fmt.Sprint(limit))
	}
	return j, c.json(ctx, &j, args...)
}

// Doctor returns `longarm doctor --json`. A failing check exits non-zero but
// still prints the report, so output is decoded when present.
func (c *Client) Doctor(ctx context.Context) (Doctor, error) {
	var d Doctor
	out, err := c.Run(ctx, 45*time.Second, nil, "doctor", "--json")
	if len(out) > 0 {
		if jerr := json.Unmarshal(out, &d); jerr == nil {
			return d, nil
		}
	}
	if err == nil {
		err = errors.New("empty doctor output")
	}
	return d, err
}

// Kill kills a job generation (`--if-created-at` guards against a reused id).
func (c *Client) Kill(ctx context.Context, worker, id, createdAt string) error {
	args := []string{"kill"}
	if worker != "" {
		args = append(args, worker)
	}
	args = append(args, id)
	if createdAt != "" && c.Has(ctx, "job-kill-if-created-at") {
		args = append(args, "--if-created-at", createdAt)
	}
	_, err := c.Run(ctx, 30*time.Second, nil, args...)
	return err
}

// Logs returns the last maxBytes of a job's log.
func (c *Client) Logs(ctx context.Context, worker, id string, maxBytes int) ([]byte, error) {
	args := []string{"logs"}
	if worker != "" {
		args = append(args, worker)
	}
	args = append(args, id, "--from", fmt.Sprint(-maxBytes))
	return c.Run(ctx, 30*time.Second, nil, args...)
}

// WorkerAdd mints a one-time code for a worker owned by the caller. The code
// is the only stdout line; help goes to stderr (upstream myworkers.go).
func (c *Client) WorkerAdd(ctx context.Context, name string, ttl time.Duration, labels []string) (secret.Secret, error) {
	args := []string{"worker", "add"}
	if name != "" {
		args = append(args, name)
	}
	args = append(args, "--ttl", ttl.String())
	for _, l := range labels {
		args = append(args, "--label", l)
	}
	out, err := c.Run(ctx, 30*time.Second, nil, args...)
	if err != nil {
		return secret.Secret{}, err
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if line == "" || strings.ContainsAny(line, " \t") {
		return secret.Secret{}, errors.New("longarm worker add printed no join code")
	}
	return secret.New(line), nil
}

// JoinCodeRequest describes `longarm admin join-code`.
type JoinCodeRequest struct {
	Role   string // "worker" (default) or "client"
	Name   string // fixed node name
	User   string // client: the Longarm user the certificate belongs to ("" = the node's own name)
	TTL    time.Duration
	Labels []string // worker only
}

// AdminJoinCode mints a one-time join code as an admin (`longarm admin
// join-code`). A worker joins pending unless the hub auto-approves; approve it
// with AdminApprove. A client code is issued without approval. Only the code
// is on stdout.
func (c *Client) AdminJoinCode(ctx context.Context, req JoinCodeRequest) (secret.Secret, error) {
	args := joinCodeArgs(req)
	out, err := c.Run(ctx, 30*time.Second, nil, args...)
	if err != nil {
		return secret.Secret{}, err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return secret.Secret{}, errors.New("longarm admin join-code printed no code")
	}
	return secret.New(fields[0]), nil
}

func joinCodeArgs(req JoinCodeRequest) []string {
	role := req.Role
	if role == "" {
		role = "worker"
	}
	args := []string{"admin", "join-code", "--role", role, "--ttl", req.TTL.String()}
	if req.Name != "" {
		args = append(args, "--name", req.Name)
	}
	if req.User != "" && role != "worker" {
		args = append(args, "--user", req.User)
	}
	if len(req.Labels) > 0 && role == "worker" {
		args = append(args, "--labels", strings.Join(req.Labels, ","))
	}
	return args
}

// AdminApprove approves a pending node.
func (c *Client) AdminApprove(ctx context.Context, name string) error {
	_, err := c.Run(ctx, 30*time.Second, nil, "admin", "approve", name)
	return err
}

// SkillAgent is one row of `longarm skill status --json`.
type SkillAgent struct {
	Agent            string `json:"agent"`
	Name             string `json:"name"`
	Detected         bool   `json:"detected"`
	Dir              string `json:"dir"`
	State            string `json:"state"` // absent | current | outdated | modified | foreign
	InstalledVersion string `json:"installed_version"`
	BundledVersion   string `json:"bundled_version"`
	Note             string `json:"note"`
}

// SkillStatus returns the per-agent state of Longarm's bundled agent skill.
func (c *Client) SkillStatus(ctx context.Context) ([]SkillAgent, error) {
	var out struct {
		Agents []SkillAgent `json:"agents"`
	}
	err := c.json(ctx, &out, "skill", "status", "--json")
	return out.Agents, err
}

// Skill runs `longarm skill install|update|remove --agent <agent>`.
func (c *Client) Skill(ctx context.Context, verb, agent string) ([]byte, error) {
	return c.Run(ctx, 60*time.Second, nil, "skill", verb, "--agent", agent)
}

// Join enrolls this machine as a client of hub (address host:port). The code
// is passed as an argument because upstream has no stdin form; it is a
// single-use, short-lived code and never logged by lazylongarm.
func (c *Client) Join(ctx context.Context, address string, code secret.Secret, fingerprint string) ([]byte, error) {
	args := []string{"join", address, code.Reveal()}
	if fingerprint != "" {
		args = append(args, "--ca-fingerprint", fingerprint)
	}
	out, err := c.Run(ctx, 90*time.Second, nil, args...)
	if err != nil {
		return out, errors.New(secret.Redact(err.Error(), code))
	}
	return []byte(secret.Redact(string(out), code)), nil
}

// DaemonStart starts the background client daemon (idempotent upstream).
func (c *Client) DaemonStart(ctx context.Context) error {
	_, err := c.Run(ctx, 30*time.Second, nil, "daemon", "start")
	return err
}

// Exec runs a short command on worker and returns its stdout.
func (c *Client) Exec(ctx context.Context, worker string, argv ...string) ([]byte, error) {
	args := append([]string{"exec", worker, "--timeout", "30s", "--"}, argv...)
	return c.Run(ctx, 60*time.Second, nil, args...)
}
