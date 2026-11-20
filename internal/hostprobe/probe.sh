# lazylongarm host probe: read-only, POSIX sh, prints key=value lines.
# Never prompts: sudo is only tried with -n.
PATH="$HOME/.local/bin:$HOME/bin:/usr/local/bin:/opt/homebrew/bin:/usr/sbin:/sbin:$PATH"
LC_ALL=C; export LC_ALL
kv() { printf '%s=%s\n' "$1" "$2"; }
has() { command -v "$1" >/dev/null 2>&1; }
kv os "$(uname -s 2>/dev/null)"
kv arch "$(uname -m 2>/dev/null)"
kv kernel "$(uname -r 2>/dev/null)"
if [ -r /etc/os-release ]; then
  . /etc/os-release 2>/dev/null
  kv distro_id "${ID:-}"; kv distro_version "${VERSION_ID:-}"; kv distro_codename "${VERSION_CODENAME:-}"; kv distro_pretty "${PRETTY_NAME:-}"
elif has sw_vers; then
  kv distro_id macos; kv distro_version "$(sw_vers -productVersion 2>/dev/null)"; kv distro_pretty "macOS $(sw_vers -productVersion 2>/dev/null)"
fi
kv user "$(id -un 2>/dev/null)"
kv uid "$(id -u 2>/dev/null)"
kv groups "$(id -Gn 2>/dev/null)"
kv home "$HOME"
kv hostname "$(hostname 2>/dev/null)"
if has sudo; then
  if sudo -n true >/dev/null 2>&1; then kv sudo nopasswd; else kv sudo password; fi
else
  kv sudo absent
fi
for c in curl wget git chezmoi systemctl launchctl docker apt-get dnf gpg tailscale python3 ss lsof sha256sum shasum; do
  if has "$c"; then kv "has_$c" yes; fi
done
if has tailscale; then kv tailscale_ip "$(tailscale ip -4 2>/dev/null | head -n1)"; fi
ver() { "$1" version 2>/dev/null | head -n1 || true; }
for b in longarm longarmd longarm-hub; do
  p=$(command -v "$b" 2>/dev/null || true)
  if [ -n "$p" ]; then
    kv "bin_$b" "$p"
    v=$("$p" --version 2>/dev/null | head -n1)
    [ -n "$v" ] || v=$(ver "$p")
    kv "ver_$b" "$v"
  fi
done
if [ -d "$HOME/.longarm/tls" ]; then kv worker_enrolled yes; fi
if [ -r "$HOME/.longarm/hub.toml" ]; then
  grep -E '^[[:space:]]*(address|addr|hub|fingerprint|ca_fingerprint)[[:space:]]*=' "$HOME/.longarm/hub.toml" 2>/dev/null | head -n4 | while IFS= read -r line; do kv worker_hub "$line"; done
fi
if [ -d "$HOME/.config/longarm/tls" ]; then kv client_enrolled yes; fi
if has systemctl; then
  kv svc_longarmd "$(systemctl is-active longarmd 2>/dev/null || true)"
  kv svc_longarmd_user "$(systemctl is-active "longarmd@$(id -un)" 2>/dev/null || true)"
  kv svc_longarm_hub "$(systemctl is-active longarm-hub 2>/dev/null || true)"
fi
if has launchctl; then
  if launchctl print system/com.longarm.longarmd >/dev/null 2>&1; then kv svc_longarmd active; fi
fi
if has dpkg-query; then
  v=$(dpkg-query -W -f='${Status} ${Version}' longarm-hub 2>/dev/null | grep 'install ok installed' | awk '{print $NF}')
  [ -n "$v" ] && kv pkg_longarm_hub "$v"
  v=$(dpkg-query -W -f='${Status} ${Version}' longarm 2>/dev/null | grep 'install ok installed' | awk '{print $NF}')
  [ -n "$v" ] && kv pkg_longarm "$v"
fi
[ -f /etc/apt/sources.list.d/longarm.sources ] && kv apt_repo yes
if [ -r /proc/meminfo ]; then
  kv mem_total_kb "$(awk '/^MemTotal:/{print $2}' /proc/meminfo)"
  kv mem_avail_kb "$(awk '/^MemAvailable:/{print $2}' /proc/meminfo)"
elif has sysctl; then
  b=$(sysctl -n hw.memsize 2>/dev/null); [ -n "$b" ] && kv mem_total_kb $((b / 1024))
fi
if has nproc; then kv cpus "$(nproc 2>/dev/null)"; elif has sysctl; then kv cpus "$(sysctl -n hw.ncpu 2>/dev/null)"; fi
if has ss; then
  ss -H -lntu 2>/dev/null | awk '{print $1" "$5}' | sort -u | while IFS= read -r l; do kv listen "$l"; done
elif has netstat; then
  netstat -an 2>/dev/null | awk '/LISTEN/ && $1 ~ /tcp/ {print "tcp "$4}' | sort -u | while IFS= read -r l; do kv listen "$l"; done
fi
if has docker; then
  names=$(docker ps --format '{{.Names}}' 2>/dev/null || sudo -n docker ps --format '{{.Names}}' 2>/dev/null)
  for n in $names; do kv container "$n"; done
fi
kv probe_done yes
