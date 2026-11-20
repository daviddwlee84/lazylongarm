package flows

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/hostprobe"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/release"
)

// How the release binaries reach a worker (`--fetch`).
const (
	// FetchAuto: the machine downloads them from GitHub with tight bounds;
	// on any download failure this machine pushes them instead.
	FetchAuto = "auto"
	// FetchPush: this machine downloads and verifies them (cached) and
	// copies them over ssh stdin. The worker needs no GitHub access.
	FetchPush = "push"
	// FetchRemote: the machine downloads them from GitHub; a failure stops
	// the step.
	FetchRemote = "remote"
)

var (
	// remoteFetch is an explicit choice: abort only a near-stall.
	remoteFetch = connection.FetchOptions{ConnectTimeout: 30 * time.Second, SpeedLimit: 1 << 10, SpeedTime: 60 * time.Second, Timeout: time.Hour}
	// autoFetch gives up quickly, so a firewalled or slow machine falls back
	// to push after seconds, not minutes.
	autoFetch = connection.FetchOptions{ConnectTimeout: 10 * time.Second, SpeedLimit: 32 << 10, SpeedTime: 20 * time.Second, Timeout: 30 * time.Minute}
)

func validFetch(mode string) bool {
	return mode == FetchAuto || mode == FetchPush || mode == FetchRemote
}

// effectiveFetch: this machine always installs from its own verified cache.
func effectiveFetch(mode, host string) string {
	if host == "" {
		return FetchPush
	}
	return mode
}

func fetchSummary(mode, host string) string {
	switch {
	case host == "":
		return "from this machine's verified release cache"
	case mode == FetchAuto:
		return "auto: the machine downloads from GitHub (checked against SHA256SUMS here); if that fails, this machine copies them over SSH"
	case mode == FetchRemote:
		return "remote: the machine downloads from GitHub (checked against SHA256SUMS here); no fallback"
	}
	return "push: this machine downloads and verifies them, then copies them over SSH (the machine needs no GitHub access)"
}

type binary struct{ component, dest string }

// binariesStep installs longarmd + longarm into binDir on the machine, by
// push, remote download or auto (remote, else push). Each binary whose
// sha256 already matches SHA256SUMS is left alone, so a rerun after a
// half-finished pair copies only the missing one.
func binariesStep(host, where string, f *hostprobe.Facts, tag, mode, binDir string, assets []binary, deps WorkerDeps) plan.Step {
	run := func(script string, args ...string) connection.Request {
		return connection.Request{Host: host, Script: script, Args: args, Timeout: 30 * time.Second}
	}
	resolveTag := func(ctx context.Context, env *plan.Env) (string, error) {
		if t := env.Get("release_tag"); t != "" {
			return t, nil
		}
		t := tag
		if t == "" {
			var err error
			if t, err = release.Latest(ctx); err != nil {
				return "", err
			}
		}
		env.Set("release_tag", t)
		return t, nil
	}
	checksum := func(ctx context.Context, env *plan.Env, t, asset string) (string, error) {
		if deps.Checksum != nil {
			return deps.Checksum(ctx, t, asset)
		}
		local, err := deps.Fetch(ctx, t, asset, env.Logf)
		if err != nil {
			return "", err
		}
		return release.SHA256(local)
	}
	// remoteSHA is "" when the file is missing or unreadable.
	remoteSHA := func(ctx context.Context, env *plan.Env, dest string) (string, error) {
		res, err := env.RunQuiet(ctx, run(`if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi 2>/dev/null | cut -d' ' -f1`, dest))
		var ce *connection.CommandError
		if errors.As(err, &ce) {
			return "", nil
		}
		return strings.TrimSpace(string(res.Stdout)), err
	}

	names := make([]string, len(assets))
	dests := make([]string, len(assets))
	for i, a := range assets {
		names[i] = release.AssetName(a.component, f.OS, f.Arch)
		dests[i] = a.dest
	}
	urlTag := tag
	if urlTag == "" {
		urlTag = "<latest tag>"
	}
	compare := "compare the sha256 of " + strings.Join(dests, " and ") + " with SHA256SUMS; only the binaries that differ are installed"
	pushDisplay := "copy to " + strings.Join(dests, " and ") + " (0755, via ssh stdin; size + sha256 checked there before an atomic rename; fails after " + fmt.Sprintf("%.0fs", connection.PushStall.Seconds()) + " without progress)"
	var actions []plan.Action
	switch mode {
	case FetchPush:
		actions = []plan.Action{
			{Where: "local", Display: fmt.Sprintf("download %s (%s) from GitHub, verify SHA256SUMS (cached)", strings.Join(names, " and "), orLatest(tag))},
			{Where: where, Display: compare},
			{Where: where, Display: pushDisplay},
		}
	default:
		opts := autoFetch
		if mode == FetchRemote {
			opts = remoteFetch
		}
		actions = []plan.Action{
			{Where: "local", Display: fmt.Sprintf("read the sha256 of %s from SHA256SUMS of %s (downloaded from GitHub, cached)", strings.Join(names, " and "), orLatest(tag))},
			{Where: where, Display: compare},
		}
		for i := range assets {
			actions = append(actions, plan.Action{Where: where, Display: connection.FetchCommand(release.AssetURL(urlTag, names[i]), dests[i], "0755", opts)})
		}
		if mode == FetchAuto {
			actions = append(actions,
				plan.Action{Where: "local", Display: fmt.Sprintf("only if a download there fails: download %s here, verify SHA256SUMS (cached)", strings.Join(names, " and "))},
				plan.Action{Where: where, Display: "only if a download there fails: " + pushDisplay})
		}
	}

	return plan.Step{
		ID: "binaries", Title: "Install longarmd + longarm into " + binDir, Idempotent: true,
		Actions: actions,
		Check: func(ctx context.Context, env *plan.Env) (bool, error) {
			t, err := resolveTag(ctx, env)
			if err != nil {
				return false, err
			}
			for i, a := range assets {
				want, err := checksum(ctx, env, t, names[i])
				if err != nil {
					return false, err
				}
				have, err := remoteSHA(ctx, env, a.dest)
				if err != nil || have != want {
					return false, err
				}
			}
			env.Logf("  %s already has longarmd/longarm %s", f.Where(), t)
			return true, nil
		},
		Apply: func(ctx context.Context, env *plan.Env) error {
			t, err := resolveTag(ctx, env)
			if err != nil {
				return err
			}
			download := mode != FetchPush
			for i, a := range assets {
				want, err := checksum(ctx, env, t, names[i])
				if err != nil {
					return err
				}
				have, err := remoteSHA(ctx, env, a.dest)
				if err != nil {
					return err
				}
				if have == want {
					env.Logf("  %s is already in place on %s (sha256 matches)", a.dest, where)
					continue
				}
				if download {
					err := fetchOnMachine(ctx, env, host, where, mode, t, names[i], want, a.dest)
					if err == nil {
						continue
					}
					if mode == FetchRemote {
						return plan.WithFix(err, "rerun with --fetch push to copy the binaries from this machine instead")
					}
					if !fallbackOK(ctx, err) {
						return err
					}
					var fe *connection.FetchError
					if errors.As(err, &fe) && fe.Kind == connection.FetchChecksum {
						env.Logf("  ! %s received different bytes for %s than SHA256SUMS lists; nothing from that download was installed (something on its network path may alter downloads)", where, names[i])
					}
					env.Logf("  ! %s could not download %s itself (%v)", where, names[i], err)
					env.Logf("  ! falling back to push: copying the binaries from this machine over SSH")
					download = false
				}
				if err := pushToMachine(ctx, env, deps, host, where, t, names[i], want, a.dest); err != nil {
					return err
				}
			}
			return nil
		},
		Verify: func(ctx context.Context, env *plan.Env) error {
			res, err := env.RunQuiet(ctx, run(`"$1" --version 2>/dev/null || "$1" version`, assets[0].dest))
			if err != nil {
				return fmt.Errorf("installed longarmd does not run on %s: %w", f.Where(), err)
			}
			env.Logf("  longarmd %s", hostprobe.ExtractVersion(strings.TrimSpace(string(res.Stdout))))
			return nil
		},
	}
}

// fetchOnMachine has the machine download one asset and verify it against want.
func fetchOnMachine(ctx context.Context, env *plan.Env, host, where, mode, tag, asset, want, dest string) error {
	opts := autoFetch
	if mode == FetchRemote {
		opts = remoteFetch
	}
	env.Logf("  %s downloads %s %s from GitHub (expects sha256 %s…)", where, asset, tag, want[:16])
	start := time.Now()
	if err := connection.FetchFile(ctx, host, release.AssetURL(tag, asset), want, dest, "0755", opts); err != nil {
		return fmt.Errorf("%s: %w", asset, err)
	}
	env.Logf("  verified and installed %s on %s (%s)", dest, where, time.Since(start).Round(time.Second))
	return nil
}

// fallbackOK: auto falls back to push when the download itself failed, not
// when ssh cannot reach the machine (push would fail the same way) or the run
// was canceled.
func fallbackOK(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var auth *connection.AuthRequiredError
	var transport *connection.SSHTransportError
	return !errors.As(err, &auth) && !errors.As(err, &transport)
}

// pushToMachine copies one verified asset from the local cache.
func pushToMachine(ctx context.Context, env *plan.Env, deps WorkerDeps, host, where, tag, asset, want, dest string) error {
	local, err := deps.Fetch(ctx, tag, asset, env.Logf)
	if err != nil {
		return err
	}
	if host == "" && samePath(local, dest) {
		return nil
	}
	data, err := os.ReadFile(local)
	if err != nil {
		return err
	}
	env.Logf("  copying %s (%.1f MB) → %s:%s", filepath.Base(local), float64(len(data))/1e6, where, dest)
	start := time.Now()
	last := start
	progress := func(n, total int64) {
		if n >= total || time.Since(last) < 15*time.Second {
			return
		}
		last = time.Now()
		rate := float64(n) / time.Since(start).Seconds() / 1e3
		env.Logf("    %.1f of %.1f MB (%d%%, %.0f KB/s)", float64(n)/1e6, float64(total)/1e6, n*100/total, rate)
	}
	if err := connection.PushFile(ctx, host, data, dest, "0755", connection.PushOptions{SHA256: want, Progress: progress}); err != nil {
		return fmt.Errorf("copy %s to %s: %w", asset, where, err)
	}
	return nil
}
