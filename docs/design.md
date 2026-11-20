# Design

## Layers

```
cmd/lazylongarm        → internal/cli.Execute (exit codes, JSON error envelope)
internal/cli           Cobra tree; wizards = sequences of internal/wizard forms
internal/tui           dashboard; wizards run in-process via tea.Exec handoff
internal/ui/theme      semantic styles (accent, muted, ok/warn/err, chips, boxes) shared by tui, wizard, apply output
internal/flows         HubRequest/WorkerRequest + facts → plan.Plan (Build*, Validate*)
internal/plan          Step{Check,Apply,Verify}, Runner, Journal, digest, Render/JSON
internal/connection    system ssh/sh runner, sudo -n + interactive handoff, tunnels
internal/hostprobe     POSIX sh probe → Facts (no python assumed)
internal/longarm       Longarm CLI adapter over the stable --json contract
internal/hubhost       hub.toml render, bootstrap.env / ca show parsing
internal/cloudfw       Azure NSG discovery and rule planning via `az`
internal/release       GitHub release download + SHA256SUMS verification + cache
internal/fleet         hub registry ⨝ managed targets; hub status checks
internal/companion     hc/herdr machine lists via their CLIs; chezmoi state script, bootstrap templates
internal/config        XDG paths, TOML config; locked read-modify-write updates
internal/secret        redacting wrapper for join codes / TOTP secrets
```

## Principles

- **One planner, many entry points.** CLI flags, the wizard and the dashboard
  all build the same `plan.Plan`; Review, `--dry-run` and `--dry-run --json`
  render the same actions; Apply refuses a plan whose digest changed.
- **Check → Apply → Verify.** Steps detect existing effects, so reruns resume.
  Non-idempotent steps that end with an unknown outcome block the next run
  until the user inspects (`--force-resume`).
- **Longarm's contract only.** Exit codes, `--json`, feature names
  (docs/programs.md upstream). The daemon's `/v1/*` API and data directories
  are internal and unused. Fixtures from upstream are vendored in
  `internal/longarm/testdata/upstream`.
- **OpenSSH is the authority.** No SSH config parsing except `ssh -G` for
  ControlMaster fields and Host aliases as picker suggestions. Background
  calls use `BatchMode=yes`, `StrictHostKeyChecking=yes`,
  `ClearAllForwardings=yes`, `ForwardAgent=no`; prompts become typed errors
  and are handed to the terminal. (Adapted from lazyclash.)
- **sudo per step.** `sudo -n` first; on "password required" the same step
  runs interactively with `ssh -tt` (sudo timestamps are per TTY).
  Secret-returning reads never fall back to an interactive run.
- **Secrets travel on stdin.** `longarmd join` receives the code from a fixed
  `read -r code` wrapper over ssh stdin. `longarm join` (local in hub setup,
  remote in peer add) has to take it as an argument, because upstream has no
  stdin form: on a remote machine the wrapper reads it from ssh stdin and puts
  it only on that `longarm join`'s argv. It is single-use and valid 15 min.
- **XDG everywhere**, including macOS, for lazylongarm's own files.
- **Config writes are short read-modify-writes.** Commands load the config at
  startup but record results through `config.Update`: take the lock, re-read
  the file, change only this operation's entry (one target, one hub), write,
  release. Overlapping long runs (two `worker add`s) both keep their entries,
  and a retried step starts from the current file.
- **The config is shared across builds.** A release, a global install and a
  dev build may all use one file, so keys this build does not know are not an
  error: they load with one stderr warning (newer lazylongarm, or a typo from
  hand-editing) and are written back as they were. Unknown keys in `[[hubs]]`
  and `[[targets]]` follow their entry by `id`, so deleting an entry deletes
  them too. When unknown keys are present the file is written from maps, so
  keys come out sorted; otherwise struct order is kept. Malformed TOML, type
  mismatches and failed validation still refuse to load.
- **One layout for drawing and clicking.** The dashboard and the forms compute
  a pure layout (pane rectangles, rows, footer hints, buttons) that both `View`
  and the mouse handler use. Tabs and rows act on press; actions and buttons
  act on release, only on the same target with unchanged state; an open modal
  takes every click. Meaning never depends on color alone: focus (`┃`, `▌`),
  the option cursor (`›`) and chosen values (`●`, `[x]`) have distinct markers.

## Companions

`companion remotes|dotfiles` reuse the plan engine: `flows.BuildCompanionRemotes`
(local steps; Check = the tool already lists the machine) and
`flows.BuildCompanionDotfiles` (bootstrap or update over `ssh -tt` → verify →
record on the target). Other tools' inventories are read through their public
`--json` output only. Personal values (repository, bundles, a bootstrap command)
live in the user's config; the code and docs carry only placeholders.

## Not done (see TODO)

Release channel and `upgrade`, minisign verification, non-apt hub hosts,
private-network hub recipe automation, Windows workers, more cloud providers,
`hub uninstall`.
