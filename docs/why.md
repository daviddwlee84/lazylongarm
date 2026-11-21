# Why lazylongarm

[Longarm](https://github.com/lubobill1990/longarm) ships three programs: the
`longarm-hub` coordinator, the `longarmd` worker daemon and the `longarm` client.
They are everything a fleet needs at runtime. What Longarm leaves to you is getting
machines into that state and keeping an eye on them. You install the hub, enroll
each machine, move join codes around, open firewall ports and remember what you did
where. lazylongarm does that part. For running jobs you keep using Longarm.

## What you would do by hand

Setting up a hub and one worker with Longarm alone looks roughly like this.

**On the hub host**

- Add Longarm's apt repository after checking its signing key's fingerprint.
- Install `longarm-hub` and write `hub.toml` (public address, ports, certificate
  lifetimes, approval policy).
- Bootstrap the CA as the service user and start the systemd unit.
- Open 7443/tcp and 7444/udp in the cloud firewall, keeping the panel on loopback.

**On your laptop**

- Download the client, read the CA fingerprint from the hub and enroll as admin.

**On every worker**

- Download the right `longarmd`/`longarm` build for its OS and architecture and
  verify the checksums.
- Copy the binaries over.
- Mint a join code and get it to the machine without leaving it in a shell history.
- Run `longarmd join` with the CA fingerprint, approve the worker and install the
  service (sudo).
- Wait until it shows up online.

Then repeat for the next machine, and again whenever something has to be repaired.
None of this is hard, but it is long, easy to get slightly wrong, and it happens
rarely enough that you never remember the exact steps.

## What lazylongarm adds

| Area | Longarm alone | With lazylongarm |
|---|---|---|
| Hub install | manual apt, config, bootstrap, systemd, firewall | `hub setup`: one reviewed plan, including Azure NSG rules and enrolling this machine as admin |
| Adding workers | per-machine downloads, copies, join codes, approval, service | `worker add <ssh-host>`: verified binaries (downloaded there or pushed over SSH), code passed on ssh stdin, approval, service, waits until online |
| Seeing what will happen | read the docs, run commands one by one | the Review screen shows every command, file and port (`#` = root) before anything changes; `--dry-run [--json]` prints the same plan |
| Failures halfway | work out which steps already ran | every step is Check → Apply → Verify with a journal: rerunning skips what is in place and resumes |
| Secrets | join codes and TOTP secrets pass through your terminal and clipboard | join codes go over ssh stdin and never reach logs, the Review screen or the config; output is redacted |
| Prompts | you juggle ssh and sudo sessions | lazylongarm runs batch SSH, and when ssh or sudo needs you it hands over the terminal, so it never sees passwords |
| Watching the fleet | `longarm worker ls` / `jobs` / `logs` in separate commands | a lazygit-style dashboard: workers with CPU, memory, path and labels; jobs with a live log tail, follow and guarded kill; hub checks and `longarm doctor` |
| Hub operations | SSH in for status, backups, panel accounts | `hub status`, `hub backup`, `hub panel` (SSH tunnel to the loopback-only panel), `hub panel-user` (password + TOTP) |
| Hub VM (Azure) | Azure portal or `az` | `hub vm status/start/stop/restart/delete` with traffic and a monthly cost estimate; deletion is guarded by a backup check |
| Tools around the fleet | set each machine up separately | `companion remotes` registers machines in hc and herdr; `companion dotfiles` bootstraps chezmoi dotfiles on them ([operating guide](operating-guide.md#companions)) |
| Coding agents | write your own instructions | `--json` everywhere with a stable error envelope, plus an embedded skill (`lazylongarm --skill`, `skill sync`) |

## What lazylongarm deliberately does not do

- **It does not replace Longarm's CLI for work.** `longarm run`, `exec`, `jobs`,
  `logs`, `forward` and `link` stay the way to use the fleet, and their `--json`
  output is the contract lazylongarm builds on too.
- **It does not touch Longarm's internals.** No daemon API, no data directories. The
  fleet keeps working if you uninstall lazylongarm.
- **It does not keep secrets.** The config holds hosts, names and fingerprints only.
- **It does not parse your SSH config.** OpenSSH is the authority (`ssh -G`, your
  aliases, agents and ControlMaster settings).

## When you can skip it

- You joined a hub someone else runs and only need a client: `longarm join` is enough.
- You run one machine and are happy with `longarm` commands.
- You only submit jobs: use `longarm` directly. The dashboard is optional.
