# Future work

Shipped in the first slice: `hub setup` (apt + systemd + Azure NSG + local admin
enrollment), `hub status|panel|panel-access|backup`, `worker add` (SSH or local; system or
user service), `worker ls`, `status`, dashboard (Fleet/Jobs/Hub), embedded skill.

- **Hub VM in the dashboard (P3, S)**: `hub vm` exists (status/start/stop/
  restart/delete); add Hub-tab actions and show the cost estimate there.
- **"Create a dedicated hub VM" in the hub setup wizard (P3, L)**: automate a
  dedicated small VM (B1ms Ubuntu LTS, free DNS label via
  `--public-ip-address-dns-name`, SSH key, host-key fingerprint fetched with
  `az vm run-command` before the first SSH). Reuse the shape of lazyclash's
  `vps create` (quote → preview digest → apply → resume). Same machinery later
  for ephemeral cloud workers.
- **Homebrew (P2, S)**: add a `tools.json` entry to `daviddwlee84/homebrew-tap`
  (archive `lazylongarm_{tag}_{os}_{arch}.tar.gz`, checksums `checksums.txt`,
  bundled completions); `upgrade` already delegates to `brew upgrade` for
  Cellar installs.
- **minisign verification of Longarm binaries (P2, S)**: SHA256SUMS is fetched
  from the same GitHub release; pin the release public key (built into
  longarmd, not published in the repo) and check `.minisig` with name+version
  in the trusted comment. → [backlog](backlog/minisign-verification.md)
- **Private-network hub mode, end to end (P2, M)**: the plan supports
  `--mode private --address <tailnet-ip>`; add a guided choice of a tailnet
  machine, skip the cloud firewall, and document moving a hub (CA import).
- **Existing / public hub mode (P2, L)**: today every flow assumes a hub we
  operate over SSH (admin join codes, `hub.toml`, panel). Add `hub use <url>`
  (config mode `existing`, CA pinned from the hub), enroll this machine with
  `longarm login` and workers as owned workers via `longarmd login` (approve at
  `/link`), and hide hub-host actions (setup, panel-user, panel-access, backup,
  vm). Document the trust model first: the hub operator sees job metadata and,
  by default (`[hub] remote_jobs = "control"`), can kill jobs and read logs;
  offer `[access] hub_job_details = false` and `[update] disabled = true`.
- **Sign-in providers for the public panel (P3, M)**: `hub panel-access public`
  uses password + TOTP only. Add an optional `[auth.google]` / `[auth.microsoft]`
  block: guide creating the OAuth client (redirect URI
  `<public_url>/auth/callback/<provider>`), pass the client secret over stdin to
  a root:longarm-hub 0640 file, set `admins` / `allowed`.
- **Later devices of a user via `/link` (P2, M)**: once a user has a device,
  upstream refuses user-bound codes; `peer add` now stops with the manual
  steps. Automate them: run `longarm login --no-browser` on the machine (over
  SSH), open the panel's `/link` (tailnet or SSH tunnel, not the URL login
  prints), show the code and wait for approval. Approving needs the user's own
  panel login (made with its first device) or an admin login named like the
  user (the device becomes an admin): say which before starting.
- **A reachable `/link` URL from `longarm login` (P3, S)**: the hub prints
  `public_url` + `/link`, else the gRPC host with the HTTP port, which fails
  for a loopback panel. Evaluate setting `public_url` from `hub panel-access`
  (tailnet/public); check what else it changes (install URLs, tailnet config).
- **Pin `[access]` and end-to-end trust on owned workers (P2, M)**: after
  `worker add --owner`, offer to write `[access] owners/users` (and
  `hub_job_details = false`) into `longarmd.toml`, `longarmd trust add-user`
  with the owner's key, and restart; refuse to pin a user this machine's
  certificate does not act as (it would lock you out).
- **Revoke/remove from lazylongarm (P2, S)**: dashboard action + `worker remove`
  wrapping `longarm admin revoke` / `worker remove`, then offer
  `worker add --reenroll`; a guided "lost admin device" flow (mint an admin
  code on the hub over SSH, enroll a new device, revoke the old one).
- **`hub uninstall` and `worker remove` (P2, M)**: stop/disable, `longarm worker
  remove`, delete only `lazylongarm-*` NSG rules, offer data deletion only
  after a backup.
- **Ephemeral cloud workers (P3, L)**: cloud-init/`--service user` bootstrap with
  short TTL codes, `longarmd leave` + destroy; separate "disposable" hub or
  labels. → [backlog](backlog/ephemeral-cloud-workers.md)
- **Ports/forwards tab (P3, M)**: `longarm forward --list/--bg/--stop`, `link ls`,
  open in browser; Web preview if upstream #159 lands.
- **Non-apt hub hosts (P3, M)**: bare `longarm-hub-linux-*` + the upstream
  unit; dnf repo for Fedora/RHEL.
- **Import hosts from other inventories (P3, S)**: read `dev fleet config show` /
  `remotes.toml` + `remotes.d/`, hc/herdr machine lists, and a dotfiles fleet
  inventory as picker suggestions; never copy their schemas.
- **Companion tab in the dashboard (P3, M)**: the `companion status` matrix
  (hc / herdr / dotfiles per machine) with the `R`/`D` actions and a bulk
  "register all missing".
- **Longarm transport in the dashboard (P3, S)**: let the Fleet tab's `R`
  choose `ssh` or `longarm` (`companion remotes --transport longarm`), and
  `--all` include hub workers lazylongarm does not manage (`longarm ls --json`).
- **Dotfiles prompt wizard (P4, M)**: read the `prompt*Once` calls from the
  repository's `.chezmoi.toml.tmpl` and offer them as a form that writes a
  preset; `ask` mode (chezmoi's own prompts) covers this today. It would also
  catch preset keys written as variable names instead of question texts
  (chezmoi silently ignores those).
- **Companions on workers, not just here (P3, M)**: register fleet machines in
  hc/herdr *on* a worker (mesh), e.g. `companion remotes --on <machine>`.
- **Privileged steps on workers (P3, M)**: jobs run as the longarmd service
  account without a TTY, so `sudo` fails and Longarm never elevates (Windows
  has upstream admin workers; Linux/macOS have nothing). Offer a helper that
  tries `sudo -n` through `longarm exec` and falls back to an attach session
  where the human types the password (the handoff the setup flows already do
  over SSH), and an opt-in `worker add` step that writes a reviewed sudoers
  allowlist for the service account. Never pipe a password to an agent.
- **Worker posture check (P2, M)**: one `longarm exec` probe per worker,
  shown by `status --doctor` (or a `lazylongarm audit`): the account jobs run
  as, isolation mode, system vs user service; root-equivalent groups
  (`docker`, `lxd`, `libvirt`, `disk`) and `sudo -n`; sensitive files that
  account can read (`~/.ssh`, cloud and `gh` credentials, agent tokens,
  `~/.config/longarm` from `peer add`); fleet vs owned, owner, shares,
  `[access]`, end-to-end trust; several Longarm config/data dirs on one
  machine. Flag the combination "client certificate + root-equivalent" (one
  compromised job reaches the whole fleet). `peer add` already warns using
  the probe's groups (`Facts.RootEquivalent`).
- **Multi-hub profiles (P4, M)**: one machine in a personal and a team hub:
  `--profile` mapping to `LONGARM_CONFIG_DIR` for the client side and a second
  `longarmd service install --name … --data-dir … --listen …` (under another
  OS account) for the worker side, with a hub switcher in the dashboard.
  Unclear how useful it is, and hard to test with a single hub; revisit when
  a second hub exists.
- **GPU column in the Fleet tab (P3, S)**: Longarm's metrics stop at CPU and
  memory (GPU scheduling is an upstream non-goal). Poll `longarm exec <w> --
  nvidia-smi --query-gpu=name,utilization.gpu,memory.used,memory.total,temperature.gpu
  --format=csv,noheader,nounits` on workers that have it (slowly, cached) and
  show utilization / VRAM; offer to set a `gpu=<model>` label from it.
- **HTTPS bridge for Anthropic-hosted Claude sessions (P3, L)**: remote MCP
  connector in front of the hub. → [backlog](backlog/https-bridge.md)
- **Authenticated MCP gateway on a trusted LAN (P4, M)**: `longarm mcp` behind
  TLS + auth with a narrowed tool set and its own non-admin identity, for
  agents that should not hold a client certificate (devcontainers, CI,
  throwaway VMs). → [backlog](backlog/https-bridge.md#lan-variant)
- **Config comment preservation (P3, S)**: the config is app-written today;
  copy lazyclash `preserve.go` if users start hand-editing it.
- **Mouse support in the dashboard (P4, S)**: pure layout → hit map, as in lazyclash.

Upstream issues worth filing (Longarm): `longarm skill print [reference]` /
`--skill` to stdout (herdr/lazyclash style, lets a stub skill defer to the
binary; low priority since copies already auto-refresh per release);
`longarmd join --code-stdin` and `longarm join --code-stdin` (`peer add`
and `hub setup` must put the client code on argv);
`speedtest` missing from `cliFeatures`; `.deb` asset names differ from the docs
(`longarm-linux-amd64.deb` vs `longarm_<v>_<arch>.deb`). Already filed: #159
(web previews), #160 (XDG), #161 (worker completion for `ssh`, `desktop`,
`speedtest`, `cp`, `agent-sessions`, `handoff`), #162 (count more coding
agents than Claude Code / Copilot CLI), #163 (GPU metrics, visibility only),
#164 (`longarm ssh` sets no `SSH_*` / `LONGARM_WORKER`, prompts look local).
