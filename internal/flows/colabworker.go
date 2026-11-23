package flows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/colab"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/release"
)

// A Colab worker is a Colab VM (a `colab` CLI session) running longarmd in
// the background, enrolled as a worker owned by the caller. The session has
// the worker's name, so `worker colab rm <name>` finds both. Colab VMs are
// disposable: nothing is recorded in lazylongarm's config.

// ColabProviderLabel marks workers made by `worker colab add`; `rm` refuses
// to revoke a worker without it.
const ColabProviderLabel = "provider=colab"

// colabRoot is longarmd's home on the VM (/content is the kernel's cwd and
// survives kernel restarts, not the VM).
const colabRoot = "/content/longarm"

// Colab is the colab CLI (internal/colab.Client).
type Colab interface {
	Usage(ctx context.Context) (colab.Usage, error)
	Sessions(ctx context.Context) ([]colab.Session, error)
	New(ctx context.Context, name, gpu string, highMem bool) error
	Exec(ctx context.Context, name, source string, silence time.Duration) (string, error)
	Upload(ctx context.Context, name, local, remote string) error
	Stop(ctx context.Context, name string) error
}

var _ Colab = (*colab.Client)(nil)

// ColabWorkerRequest describes `worker colab add`.
type ColabWorkerRequest struct {
	Name    string // worker and session name
	GPU     string // "" = CPU runtime; T4, L4, A100, ...
	HighMem bool   // high-RAM shape
	// Fetch is how longarmd reaches the VM: FetchAuto (default: the VM
	// downloads it from GitHub, checked against SHA256SUMS fetched here;
	// upload on failure), FetchRemote (no fallback) or FetchPush (upload).
	Fetch   string
	Labels  []string // KEY=VALUE, besides provider=colab and gpu=
	Hub     *config.Hub
	Version string      // longarmd release tag
	Usage   colab.Usage // the account's balance when the plan was built
}

// ColabDeps are the side effects of the Colab plans.
type ColabDeps struct {
	Client LongarmClient
	Colab  Colab
	// Fetch returns a verified local copy of a release asset.
	Fetch func(ctx context.Context, tag, asset string, logf func(string, ...any)) (string, error)
	// Checksum returns an asset's sha256 from the release's SHA256SUMS.
	Checksum     func(ctx context.Context, tag, asset string) (string, error)
	PollInterval time.Duration
	WaitTimeout  time.Duration
}

// ColabLabels are the labels a Colab worker joins with.
func ColabLabels(req ColabWorkerRequest) []string {
	out := []string{ColabProviderLabel}
	if req.GPU != "" {
		out = append(out, "gpu="+strings.ToLower(req.GPU))
	}
	for _, l := range req.Labels {
		k, _, _ := strings.Cut(l, "=")
		if k == "provider" || (k == "gpu" && req.GPU != "") {
			continue
		}
		out = append(out, l)
	}
	return out
}

// DefaultColabName is colab-<month><day>-<hour><minute> in local time.
func DefaultColabName(now time.Time) string { return "colab-" + now.Format("0102-1504") }

// ValidateColabWorker checks the request.
func ValidateColabWorker(req ColabWorkerRequest) error {
	var problems []string
	if req.Hub == nil {
		problems = append(problems, "no hub configured: run `lazylongarm hub setup` (or join a hub) first")
	} else if req.Hub.CAFingerprint == "" {
		problems = append(problems, "the hub profile has no CA fingerprint")
	}
	if !workerNameRE.MatchString(req.Name) {
		problems = append(problems, fmt.Sprintf("worker name %q must be letters, digits, '.', '_' or '-'", req.Name))
	}
	for _, l := range req.Labels {
		if !strings.Contains(l, "=") {
			problems = append(problems, fmt.Sprintf("label %q must be KEY=VALUE", l))
		}
	}
	if req.Fetch != "" && !validFetch(req.Fetch) {
		problems = append(problems, fmt.Sprintf("fetch %q must be auto, push or remote", req.Fetch))
	}
	if req.Usage.Balance <= 0 {
		// Colab FAQ: remote control (SSH-like shells) and distributed
		// computing workers are disallowed on runtimes "running free of
		// charge, without a positive Colab compute unit balance".
		problems = append(problems, fmt.Sprintf("your Colab compute-unit balance is %.2f: Colab allows remote shells and distributed-computing workers only with a positive balance (a paid plan, incl. Google AI plans); see `colab usage`", req.Usage.Balance))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// BuildColabWorkerAdd assembles `worker colab add`.
func BuildColabWorkerAdd(req ColabWorkerRequest, deps ColabDeps) (*plan.Plan, error) {
	if err := ValidateColabWorker(req); err != nil {
		return nil, err
	}
	if req.Fetch == "" {
		req.Fetch = FetchAuto
	}
	if deps.PollInterval == 0 {
		deps.PollInterval = 3 * time.Second
	}
	if deps.WaitTimeout == 0 {
		deps.WaitTimeout = 2 * time.Minute
	}
	hub, name, tag := req.Hub, req.Name, req.Version
	labels := ColabLabels(req)
	runtime := "CPU"
	if req.GPU != "" {
		runtime = req.GPU + " GPU"
	}
	if req.HighMem {
		runtime += ", high-RAM"
	}
	bin := colabRoot + "/bin/longarmd"
	data := colabRoot + "/data"
	where := "colab:" + name

	p := &plan.Plan{
		Kind:  "colab-worker-add",
		Key:   name,
		Title: fmt.Sprintf("Borrow a Colab VM (%s) as Longarm worker %q", runtime, name),
		Summary: [][2]string{
			{"colab session", fmt.Sprintf("%s (%s)", name, runtime)},
			{"compute units", fmt.Sprintf("balance %.2f, now burning %.2f/hr across %d VM(s)", req.Usage.Balance, req.Usage.RatePerHour, req.Usage.Active)},
			{"worker name", name},
			{"hub", hub.Address + " (" + hub.ID + ")"},
			{"binary", fmt.Sprintf("longarmd %s → %s; %s", orLatest(tag), bin, colabFetchSummary(req.Fetch))},
			{"daemon", "background `longarmd run` on the VM (no service; ends with the VM)"},
			{"reachable", "via the hub relay (outbound only)"},
			{"ownership", "your own worker: only you can run jobs on it"},
			{"labels", strings.Join(labels, ", ")},
		},
		Warnings: []string{
			"the VM bills compute units until you remove it: lazylongarm worker colab rm " + name,
			"Colab reclaims VMs (at most ~12 h; idle runtimes sooner) - jobs running then are lost and their output with them: copy results off before",
			"jobs you start without a worker (`--on auto`) may be placed on it: pass --on/--require for other work while it exists",
			"Colab forbids using a runtime as a proxy on every plan: forward only to your own debugging ports",
		},
	}

	remote := func(ctx context.Context, env *plan.Env, step, source string) (string, error) {
		out, err := deps.Colab.Exec(ctx, name, source, 2*time.Minute)
		if err != nil {
			return "", err
		}
		res, ok := colab.Result(out)
		if !ok {
			return "", fmt.Errorf("%s on the Colab VM did not report success:\n%s", step, tail(out, 1500))
		}
		return res, nil
	}

	// 1. session
	p.Steps = append(p.Steps, plan.Step{
		ID: "session", Title: "Create the Colab session (VM)", Idempotent: true,
		Actions: []plan.Action{{Where: "local", Display: colabNewDisplay(req)}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			list, err := deps.Colab.Sessions(ctx)
			if err != nil {
				return false, err
			}
			s, ok := colab.FindSession(list, name)
			if ok {
				env.Logf("  session %s exists (%s)", name, dash(s.Hardware))
			}
			return ok, nil
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			if err := deps.Colab.New(ctx, name, req.GPU, req.HighMem); err != nil {
				return &plan.UnknownError{Err: err}
			}
			env.Logf("  session %s is ready", name)
			return nil
		},
	})

	// 2. binary
	var arch, asset string
	probe := func(ctx context.Context, env *plan.Env) (sha string, err error) {
		res, err := remote(ctx, env, "probe", pyProbe(bin))
		if err != nil {
			return "", err
		}
		f := strings.Fields(res)
		if len(f) != 2 {
			return "", fmt.Errorf("unexpected probe result %q", res)
		}
		switch f[0] {
		case "x86_64", "amd64":
			arch = "amd64"
		case "aarch64", "arm64":
			arch = "arm64"
		default:
			return "", fmt.Errorf("unsupported Colab VM architecture %s", f[0])
		}
		asset = release.AssetName("longarmd", "linux", arch)
		return f[1], nil
	}
	p.Steps = append(p.Steps, plan.Step{
		ID: "binary", Title: "Install a verified longarmd on the VM", Idempotent: true,
		Actions: colabBinaryActions(req.Fetch, tag, bin, where),
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			have, err := probe(ctx, env)
			if err != nil || have == "-" || deps.Checksum == nil {
				return false, err
			}
			want, err := deps.Checksum(ctx, tag, asset)
			if err != nil {
				return false, nil
			}
			return have == want, nil
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			if asset == "" {
				if _, err := probe(ctx, env); err != nil {
					return err
				}
			}
			if req.Fetch != FetchPush {
				err := func() error {
					if deps.Checksum == nil {
						return errors.New("no SHA256SUMS lookup")
					}
					want, err := deps.Checksum(ctx, tag, asset)
					if err != nil {
						return err
					}
					if _, err := remote(ctx, env, "download", pyDownload(release.AssetURL(tag, asset), bin, want)); err != nil {
						return err
					}
					env.Logf("  the VM downloaded %s (%s) from GitHub, sha256 %s… matches SHA256SUMS", asset, orLatest(tag), want[:12])
					return nil
				}()
				if err == nil {
					return nil
				}
				if req.Fetch == FetchRemote {
					return err
				}
				env.Logf("  ! the VM could not download it from GitHub (%v); uploading it from here instead", firstLine(err.Error()))
			}
			local, err := deps.Fetch(ctx, tag, asset, env.Logf)
			if err != nil {
				return err
			}
			want, err := release.SHA256(local)
			if err != nil {
				return err
			}
			if err := deps.Colab.Upload(ctx, name, local, bin+".part"); err != nil {
				return err
			}
			if _, err := remote(ctx, env, "install", pyInstall(bin, want)); err != nil {
				return err
			}
			env.Logf("  uploaded %s (%s, sha256 %s…)", asset, orLatest(tag), want[:12])
			return nil
		},
	})

	// 3. enroll
	p.Steps = append(p.Steps, plan.Step{
		ID: "enroll", Title: "Enroll the worker with the hub (one-time code via an uploaded file)", Idempotent: false,
		Actions: []plan.Action{
			{Where: "local", Display: fmt.Sprintf("longarm worker add %s --ttl 15m%s   → one-time code (pre-approved, owned by you)", name, labelArgs(labels))},
			{Where: where, Display: fmt.Sprintf("longarmd join %s <code from a 0600 file, deleted first> --ca-fingerprint %s --name %s --data-dir %s --no-wait", hub.Address, shortFP(hub.CAFingerprint), name, data)},
		},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			res, err := remote(ctx, env, "enroll check", pyEnrolled(data, strings.Split(hub.Address, ":")[0]))
			if err != nil {
				return false, err
			}
			if res == "enrolled" {
				env.Logf("  the VM is already enrolled with %s", hub.Address)
			}
			return res == "enrolled", nil
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			code, err := deps.Client.WorkerAdd(ctx, name, 15*time.Minute, labels)
			if err != nil {
				return err
			}
			env.SetSecret("worker_code", code)
			env.Logf("  minted a one-time worker code (valid 15 min)")
			// The code travels as a file: an exec'd cell (and its --env
			// values) stays in the session's execution log.
			f, err := os.CreateTemp("", "lazylongarm-code-*")
			if err != nil {
				return err
			}
			defer os.Remove(f.Name())
			_, werr := f.WriteString(code.Reveal())
			if cerr := f.Close(); werr != nil || cerr != nil {
				return errors.Join(werr, cerr)
			}
			codeFile := colabRoot + "/.join-code"
			if err := deps.Colab.Upload(ctx, name, f.Name(), codeFile); err != nil {
				return err
			}
			if _, err := remote(ctx, env, "join", pyJoin(bin, data, codeFile, hub.Address, hub.CAFingerprint, name)); err != nil {
				return &plan.UnknownError{Err: err}
			}
			env.Logf("  joined as %s", name)
			return nil
		},
	})

	// 4. daemon
	p.Steps = append(p.Steps, plan.Step{
		ID: "daemon", Title: "Start longarmd in the background on the VM", Idempotent: true,
		Actions: []plan.Action{{Where: where, Display: fmt.Sprintf("setsid %s run --data-dir %s >> %s/longarmd.log &", bin, data, colabRoot)}},
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			res, err := remote(ctx, env, "daemon check", pyRunning())
			return err == nil && res != "none", err
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			res, err := remote(ctx, env, "start", pyStart(bin, data, colabRoot+"/longarmd.log"))
			if err != nil {
				return err
			}
			env.Logf("  longarmd started (pid %s)", res)
			return nil
		},
	})

	// 5. online
	p.Steps = append(p.Steps, plan.Step{
		ID: "online", Title: "Wait until the hub reports the worker online", Idempotent: true,
		Actions: []plan.Action{{Where: "local", Display: fmt.Sprintf("poll `longarm worker ls --json` for %s (up to %s)", name, deps.WaitTimeout)}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			deadline := time.Now().Add(deps.WaitTimeout)
			for {
				if list, err := deps.Client.Workers(ctx); err == nil {
					if n, ok := findNode(list.Nodes, name, ""); ok && n.Online {
						env.Set("worker_path", n.Path())
						env.Logf("  %s: online", name)
						return nil
					}
				}
				if time.Now().After(deadline) {
					return plan.WithFix(fmt.Errorf("%s did not come online within %s", name, deps.WaitTimeout),
						fmt.Sprintf("echo 'print(open(\"%s/longarmd.log\").read()[-3000:])' | colab exec -s %s", colabRoot, name))
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(deps.PollInterval):
				}
			}
		},
	})

	// 6. try it
	tryArgv := []string{"uname", "-a"}
	if req.GPU != "" {
		tryArgv = []string{"nvidia-smi", "-L"}
	}
	p.Steps = append(p.Steps, plan.Step{
		ID: "try-it", Title: "Try it: run a command on the new worker", Idempotent: true, ReadOnly: true,
		Actions: []plan.Action{{Where: "local", Display: "longarm exec " + name + " -- " + strings.Join(tryArgv, " ")}},
		Apply: func(ctx context.Context, env *plan.Env) error {
			out, err := deps.Client.Exec(ctx, name, tryArgv...)
			if err != nil {
				return err
			}
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				env.Logf("  │ %s", line)
			}
			return nil
		},
	})
	return p, nil
}

// ColabRemoveRequest describes `worker colab rm`.
type ColabRemoveRequest struct {
	Name    string
	Node    *longarm.Node  // the hub's worker of that name (nil: none)
	Session *colab.Session // the Colab session of that name (nil: none)
}

// BuildColabWorkerRemove revokes the worker, then stops the VM.
func BuildColabWorkerRemove(req ColabRemoveRequest, deps ColabDeps) (*plan.Plan, error) {
	name := req.Name
	if req.Node == nil && req.Session == nil {
		return nil, fmt.Errorf("neither a worker nor a Colab session is called %s (see `lazylongarm worker colab ls`)", name)
	}
	if n := req.Node; n != nil && !hasLabel(n.Labels, ColabProviderLabel) {
		return nil, fmt.Errorf("worker %s has no %s label: it was not made by `worker colab add`; revoke it with `longarm worker remove %s` if you mean it", name, ColabProviderLabel, name)
	}
	p := &plan.Plan{Kind: "colab-worker-rm", Key: name, Title: fmt.Sprintf("Remove Colab worker %q and stop its VM", name)}
	worker, session := "none on the hub", "none"
	if n := req.Node; n != nil {
		worker = fmt.Sprintf("%s (id %s, %s)", map[bool]string{true: "online", false: "offline"}[n.Online], shortID(n.ID), ownerText(n.Owner))
		if n.JobsActive > 0 {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%d job(s) still run on %s: they end with the VM", n.JobsActive, name))
		}
	}
	if s := req.Session; s != nil {
		session = fmt.Sprintf("%s (%s)", s.ID, dash(s.Hardware))
	}
	p.Summary = [][2]string{{"worker", worker}, {"colab session", session}}
	if req.Node != nil {
		p.Steps = append(p.Steps, plan.Step{
			ID: "revoke", Title: "Revoke the worker on the hub", Idempotent: true,
			Actions: []plan.Action{{Where: "local", Display: "longarm worker remove " + name}},
			Check: func(ctx context.Context, env *plan.Env) (bool, error) {
				list, err := deps.Client.Workers(ctx)
				if err != nil {
					return false, err
				}
				_, ok := findNode(list.Nodes, name, "")
				return !ok, nil
			},
			Apply: func(ctx context.Context, env *plan.Env) error {
				err := deps.Client.WorkerRemove(ctx, name)
				if err != nil && longarm.KindOf(err) != longarm.KindNotFound {
					return err
				}
				env.Logf("  revoked %s", name)
				return nil
			},
		})
	}
	if req.Session != nil {
		p.Steps = append(p.Steps, plan.Step{
			ID: "stop", Title: "Stop the Colab session (releases the VM)", Idempotent: true,
			Actions: []plan.Action{{Where: "local", Display: "colab stop -s " + name}},
			Check: func(ctx context.Context, env *plan.Env) (bool, error) {
				list, err := deps.Colab.Sessions(ctx)
				if err != nil {
					return false, err
				}
				_, ok := colab.FindSession(list, name)
				return !ok, nil
			},
			Apply: func(ctx context.Context, env *plan.Env) error {
				if err := deps.Colab.Stop(ctx, name); err != nil {
					return err
				}
				env.Logf("  stopped session %s", name)
				return nil
			},
		})
	}
	return p, nil
}

func hasLabel(m map[string]string, kv string) bool {
	k, v, _ := strings.Cut(kv, "=")
	return m[k] == v
}

func colabFetchSummary(mode string) string {
	switch mode {
	case FetchPush:
		return "push: downloaded and verified here, then colab upload (sha256 checked again on the VM)"
	case FetchRemote:
		return "remote: the VM downloads it from GitHub, checked against SHA256SUMS fetched here; no fallback"
	}
	return "auto: the VM downloads it from GitHub, checked against SHA256SUMS fetched here; if that fails, colab upload from here"
}

func colabBinaryActions(mode, tag, bin, where string) []plan.Action {
	sums := plan.Action{Where: "local", Display: "fetch SHA256SUMS of " + orLatest(tag) + " from GitHub"}
	download := plan.Action{Where: where, Display: "download longarmd-linux-<arch> from GitHub → " + bin + " (sha256 must match)"}
	upload := plan.Action{Where: "local", Display: "download + verify longarmd-linux-<arch> here, colab upload → " + bin + " (sha256 checked again on the VM)"}
	switch mode {
	case FetchPush:
		return []plan.Action{upload}
	case FetchRemote:
		return []plan.Action{sums, download}
	}
	upload.Display = "if that fails: " + upload.Display
	return []plan.Action{sums, download, upload}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func colabNewDisplay(req ColabWorkerRequest) string {
	s := "colab new -s " + req.Name
	if req.GPU != "" {
		s += " --gpu " + req.GPU
	}
	if req.HighMem {
		s += " --high-mem"
	}
	return s
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

// py quotes a Go string as a Python literal (JSON strings are Python str
// literals).
func py(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ---- Python run on the VM's kernel (each prints colab.Marker on success) ----

func pyProbe(bin string) string {
	return `import hashlib, os, platform
os.makedirs(` + py(colabRoot+"/bin") + `, exist_ok=True)
os.makedirs(` + py(colabRoot+"/data") + `, exist_ok=True)
b = ` + py(bin) + `
sha = "-"
if os.path.exists(b):
    h = hashlib.sha256()
    with open(b, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    sha = h.hexdigest()
print(` + py(colab.Marker) + `, platform.machine(), sha)
`
}

// pyDownload fetches url into bin on the VM (Google's network: about a
// second for longarmd, against ~20 s for colab upload) and installs it only
// when its sha256 matches.
func pyDownload(url, bin, sha string) string {
	return `import hashlib, os, urllib.request
part, b = ` + py(bin+".part") + `, ` + py(bin) + `
h, n = hashlib.sha256(), 0
with urllib.request.urlopen(` + py(url) + `, timeout=30) as r, open(part, "wb") as f:
    while True:
        chunk = r.read(1 << 20)
        if not chunk:
            break
        n += len(chunk)
        if n > 200 << 20:
            raise SystemExit("download larger than 200 MiB")
        h.update(chunk)
        f.write(chunk)
if h.hexdigest() != ` + py(sha) + `:
    os.remove(part)
    raise SystemExit("sha256 mismatch: " + h.hexdigest())
os.chmod(part, 0o755)
os.replace(part, b)
print(` + py(colab.Marker) + `, "downloaded", n)
`
}

func pyInstall(bin, sha string) string {
	return `import hashlib, os
part, b = ` + py(bin+".part") + `, ` + py(bin) + `
h = hashlib.sha256()
with open(part, "rb") as f:
    for chunk in iter(lambda: f.read(1 << 20), b""):
        h.update(chunk)
if h.hexdigest() != ` + py(sha) + `:
    os.remove(part)
    raise SystemExit("sha256 mismatch after upload: " + h.hexdigest())
os.chmod(part, 0o755)
os.replace(part, b)
print(` + py(colab.Marker) + `, "installed")
`
}

func pyEnrolled(data, hubHost string) string {
	return `import os
d = ` + py(data) + `
ok = os.path.exists(d + "/tls/node.crt") and os.path.exists(d + "/hub.toml") and ` + py(hubHost) + ` in open(d + "/hub.toml").read()
print(` + py(colab.Marker) + `, "enrolled" if ok else "no")
`
}

func pyJoin(bin, data, codeFile, hub, fp, name string) string {
	return `import os, subprocess
f = ` + py(codeFile) + `
os.chmod(f, 0o600)
code = open(f).read().strip()
os.remove(f)
r = subprocess.run([` + py(bin) + `, "join", ` + py(hub) + `, code, "--ca-fingerprint", ` + py(fp) + `, "--name", ` + py(name) + `, "--data-dir", ` + py(data) + `, "--no-wait"], capture_output=True, text=True)
del code
print((r.stdout + r.stderr)[-1500:])
if r.returncode != 0:
    raise SystemExit("longarmd join failed (exit %d)" % r.returncode)
print(` + py(colab.Marker) + `, "joined")
`
}

// pyRunning prints the pid of a running `longarmd run`, or none.
func pyRunning() string {
	return `import os
pid = "none"
for p in os.listdir("/proc"):
    if not p.isdigit():
        continue
    try:
        argv = open("/proc/%s/cmdline" % p, "rb").read().split(b"\0")
    except OSError:
        continue
    if len(argv) > 1 and argv[0].endswith(b"longarmd") and argv[1] == b"run":
        pid = p
        break
print(` + py(colab.Marker) + `, pid)
`
}

func pyStart(bin, data, log string) string {
	return `import subprocess, time
log = open(` + py(log) + `, "ab")
p = subprocess.Popen([` + py(bin) + `, "run", "--data-dir", ` + py(data) + `], stdout=log, stderr=log, stdin=subprocess.DEVNULL, start_new_session=True)
time.sleep(2)
if p.poll() is not None:
    print(open(` + py(log) + `).read()[-1500:])
    raise SystemExit("longarmd exited with %s" % p.returncode)
print(` + py(colab.Marker) + `, p.pid)
`
}
