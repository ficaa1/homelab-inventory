#!/bin/sh
# Install the server locally. No credentials are accepted or printed here.
set -eu
umask 077
usage() {
  cat <<'HELP'
Usage: get.sh [options]
  --port N          Published/listening port (default 8080)
  --dir DIR         Installer files (default /opt/homelab-inventory)
  --data DIR        Persistent data (default /var/lib/homelab-inventory)
  --version vX.Y.Z  Release tag (default latest)
  --docker          Require Docker Compose
  --no-docker       Use a static binary and systemd
  --from DIR        Local release artifacts; no network downloads
  --prefix DIR      Disposable binary layout; no root, useradd or systemctl
  --uninstall       Remove server; keep data
  --purge           With --uninstall, also delete data after confirmation
  --yes             Confirm data deletion without a terminal prompt
  --help            Show this help
INSTALL_BASE_URL overrides the release download directory. For a pinned version
it must contain that version's assets; for latest it must redirect SHA256SUMS
and the archives to one consistent release. INSTALL_IMAGE overrides the
container image repository (default ghcr.io/ficaa1/homelab-inventory).
HELP
}
die() { echo "get: $*" >&2; exit 1; }
port=8080 dir=/opt/homelab-inventory data=/var/lib/homelab-inventory
version=latest mode=auto from= prefix= uninstall=0 purge=0 yes=0
port_set=0 data_set=0
# Preserve arguments only for a copyable sudo command; never eval shell text.
sudo_args=
for arg do
  quoted=$(printf '%s' "$arg" | sed "s/'/'\\\\''/g")
  sudo_args="$sudo_args '$quoted'"
done
while [ "$#" -gt 0 ]; do
  case $1 in
    --port|--dir|--data|--version|--from|--prefix)
      [ "$#" -ge 2 ] || die 'missing option value'
      case $1 in
        --port) port=$2; port_set=1;; --dir) dir=$2;;
        --data) data=$2; data_set=1;; --version) version=$2;;
        --from) from=$2;; --prefix) prefix=$2;;
      esac
      shift 2;;
    --docker) mode=docker; shift;;
    --no-docker) mode=binary; shift;;
    --uninstall) uninstall=1; shift;;
    --purge) purge=1; shift;;
    --yes) yes=1; shift;;
    --help|-h) usage; exit 0;;
    *) usage >&2; die 'unknown option';;
  esac
done
[ "$purge" -eq 0 ] || [ "$uninstall" -eq 1 ] || die '--purge requires --uninstall'
[ "$(uname -s)" = Linux ] || die 'Linux required'
# These paths are embedded in Compose and systemd. Reject syntax and traversal.
check_path() {
  case $1 in /*) ;; *) die 'paths must be absolute';; esac
  case $1 in *[!a-zA-Z0-9/_.-]*|*/../*|*/..|*/./*|*/.|*//*|*/)
    die 'paths must use plain characters without trailing slash or traversal';;
  esac
  case $1 in /|/opt|/var|/var/lib|/usr|/usr/local|/usr/share|/etc|/home|/tmp|/root)
    die 'choose a dedicated installation/data/prefix directory';;
  esac
}
check_path "$dir"
if [ -n "$prefix" ]; then
  check_path "$prefix"
  [ "$mode" != docker ] || die '--prefix requires binary mode'
  mode=binary
elif [ "$(id -u)" -ne 0 ]; then
  if command -v sudo >/dev/null 2>&1; then
    printf 'get: root required. Run:\n  curl -fsSL https://github.com/ficaa1/homelab-inventory/releases/latest/download/get.sh | sudo sh -s --%s\n' "$sudo_args" >&2
  else
    echo 'get: run as root, or use --prefix for a disposable local test' >&2
  fi
  exit 1
fi
install_dir=$prefix$dir
metadata=$install_dir/install-state
if [ -f "$metadata" ]; then
  previous_mode=$(sed -n '1p' "$metadata")
  case $previous_mode in docker|binary) ;; *) die 'invalid install-state';; esac
  if [ "$mode" = auto ]; then mode=$previous_mode; fi
  [ "$mode" = "$previous_mode" ] || die 'uninstall the previous backend before switching modes'
  if [ "$data_set" -eq 0 ]; then data=$(sed -n '2p' "$metadata"); fi
  if [ "$port_set" -eq 0 ]; then port=$(sed -n '3p' "$metadata"); fi
fi
check_path "$data"
case $data/ in "$dir/"*) die 'data must be separate from installer files';; esac
case $dir/ in "$data/"*) die 'installer files must be separate from data';; esac
case $port in ''|*[!0-9]*|0*) die 'port must be an integer from 1 to 65535';; esac
[ "${#port}" -le 5 ] && [ "$port" -le 65535 ] || die 'port out of range'
state=$prefix$data
bindir=$prefix/usr/local/bin
agents=$prefix/usr/share/homelab-inventory/agent
units=$prefix/etc/systemd/system
if [ "$mode" = auto ] && [ -n "$from" ]; then mode=binary; fi
if [ "$mode" = auto ]; then
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then mode=docker; else mode=binary; fi
fi
if [ "$mode" = docker ]; then
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 || die 'Docker Compose required'
  [ -z "$from" ] || die '--from requires --no-docker (local artifacts contain binaries)'
fi
if [ "$mode" = binary ] && [ -z "$prefix" ]; then
  command -v systemctl >/dev/null 2>&1 || die 'systemd required; use Docker Compose instead'
fi
if [ "$uninstall" -eq 1 ]; then
  # Confirm before stopping anything; piped scripts read a controlling terminal.
  if [ "$purge" -eq 1 ] && [ "$yes" -eq 0 ]; then
    printf 'Delete all inventory data in %s? Type DELETE: ' "$state" >&2
    answer=
    if [ -r /dev/tty ]; then IFS= read -r answer < /dev/tty || true; fi
    [ "$answer" = DELETE ] || die 'purge cancelled; data retained'
  fi
  if [ "$mode" = docker ]; then
    if [ -f "$install_dir/compose.yml" ]; then docker compose -p homelab-inventory -f "$install_dir/compose.yml" down; fi
  else
    if [ -z "$prefix" ] && [ -f "$units/homelab-inventory.service" ]; then systemctl disable --now homelab-inventory.service; fi
    rm -f "$bindir/homelab-inventory" "$units/homelab-inventory.service"
    rm -rf "$agents"
    if [ -z "$prefix" ]; then systemctl daemon-reload; fi
  fi
  rm -f "$install_dir/compose.yml" "$metadata"
  rmdir "$install_dir" 2>/dev/null || true
  if [ "$purge" -eq 1 ]; then
    rm -rf "$state"
    echo 'get: server and data removed'
  else
    echo "get: server removed; data retained in $state"
  fi
  exit 0
fi
command -v curl >/dev/null 2>&1 || die 'curl required for health checks and downloads'
work=$(mktemp -d)
trap 'rm -rf "$work"' 0
trap 'exit 1' HUP INT TERM
fetch() {
  if [ -n "$from" ]; then
    cp "$from/$1" "$2" || die 'local release artifact missing'
  else
    curl -fsSL --proto '=http,https' --proto-redir '=http,https' --connect-timeout 10 --max-time 180 "$base/$1" -o "$2" || die 'release download failed'
  fi
}
# Resolve latest from the checksum manifest, so binaries never need a GitHub API.
base=${INSTALL_BASE_URL:-https://github.com/ficaa1/homelab-inventory/releases/latest/download}
if [ "$version" != latest ] && [ -z "${INSTALL_BASE_URL:-}" ]; then
  base=https://github.com/ficaa1/homelab-inventory/releases/download/$version
fi
base=${base%/}
valid_version() {
  case $1 in ''|*[!a-zA-Z0-9.-]*) return 1;; esac
  printf '%s\n' "$1" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'
}
if [ "$version" != latest ]; then valid_version "$version" || die 'version must be vX.Y.Z (optional prerelease)'; fi
if [ "$mode" = binary ] || [ "$version" = latest ]; then
  fetch SHA256SUMS "$work/SHA256SUMS"
  if [ "$version" = latest ]; then
    version=$(sed -n 's/^[0-9a-f]*  homelab-inventory_\(v[^_]*\)_linux_amd64.tar.gz$/\1/p' "$work/SHA256SUMS")
    valid_version "$version" || die 'cannot resolve one latest release from SHA256SUMS'
  fi
fi
checksum() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1";
  elif command -v busybox >/dev/null 2>&1; then busybox sha256sum "$1";
  else die 'sha256sum (or busybox sha256sum) required'; fi
}
verify() {
  expected=$(awk -v path="$2" '$2 == path && length($1) == 64 && $1 !~ /[^0123456789abcdef]/ {print $1}' "$3")
  actual=$(checksum "$1"); actual=${actual%% *}
  [ -n "$expected" ] && [ "$actual" = "$expected" ] || die 'release checksum mismatch; installation unchanged'
}
if [ "$mode" = binary ]; then
  case $(uname -m) in x86_64|amd64) arch=amd64;; aarch64|arm64) arch=arm64;; *) die 'Linux amd64 or arm64 required';; esac
  archive=homelab-inventory_${version}_linux_${arch}.tar.gz
  fetch "$archive" "$work/$archive"
  verify "$work/$archive" "$archive" "$work/SHA256SUMS"
  mkdir "$work/unpack"
  # Extract only the fixed release members; never honor arbitrary archive paths.
  tar -xzf "$work/$archive" -C "$work/unpack" homelab-inventory agent/linux-amd64/discover agent/linux-amd64/agentupdate agent/linux-arm64/discover agent/linux-arm64/agentupdate agent/SHA256SUMS
  for target in linux-amd64 linux-arm64; do
    for name in discover agentupdate; do
      file=agent/$target/$name
      [ -f "$work/unpack/$file" ] && [ ! -L "$work/unpack/$file" ] || die 'invalid agent artifact'
      verify "$work/unpack/$file" "$target/$name" "$work/unpack/agent/SHA256SUMS"
    done
  done
  [ -f "$work/unpack/homelab-inventory" ] && [ ! -L "$work/unpack/homelab-inventory" ] || die 'invalid server artifact'
  # All release verification precedes writes to installed files or service state.
  if [ -z "$prefix" ] && ! id homelab-inventory >/dev/null 2>&1; then
    useradd --system --user-group --home-dir "$data" --no-create-home --shell /sbin/nologin homelab-inventory
  fi
fi
(umask 022; mkdir -p "$install_dir" "$(dirname "$state")")
mkdir -p "$state"
chmod 700 "$state"
if [ "$mode" = docker ]; then
  image=${INSTALL_IMAGE:-ghcr.io/ficaa1/homelab-inventory}
  case $image in *[!a-zA-Z0-9./:_-]*|'') die 'unsupported INSTALL_IMAGE';; esac
  chown 10001:10001 "$state"
  cat > "$install_dir/compose.yml.new" <<COMPOSE
services:
  api:
    image: $image:$version
    restart: unless-stopped
    ports:
      - "$port:8080"
    volumes:
      - "$data:/data"
COMPOSE
  # Pull before replacing a working deployment's configuration.
  docker compose -p homelab-inventory -f "$install_dir/compose.yml.new" pull
  mv -f "$install_dir/compose.yml.new" "$install_dir/compose.yml"
  docker compose -p homelab-inventory -f "$install_dir/compose.yml" up -d
  restart="docker compose -p homelab-inventory -f $install_dir/compose.yml restart"
else
  (umask 022; mkdir -p "$bindir" "$agents" "$units")
  chmod 755 "$agents" "$(dirname "$agents")"
  install_binary() {
    cp "$1" "$2.new"
    chmod 755 "$2.new"
    mv -f "$2.new" "$2"
  }
  install_binary "$work/unpack/homelab-inventory" "$bindir/homelab-inventory"
  for target in linux-amd64 linux-arm64; do
    mkdir -p "$agents/$target"
    chmod 755 "$agents/$target"
    for name in discover agentupdate; do install_binary "$work/unpack/agent/$target/$name" "$agents/$target/$name"; done
  done
  cp "$work/unpack/agent/SHA256SUMS" "$agents/SHA256SUMS"
  chmod 644 "$agents/SHA256SUMS"
  capabilities=
  if [ "$port" -lt 1024 ]; then capabilities=CAP_NET_BIND_SERVICE; fi
  cat > "$units/homelab-inventory.service" <<UNIT
[Unit]
Description=Homelab inventory server
Wants=network-online.target
After=network-online.target
[Service]
User=homelab-inventory
Group=homelab-inventory
ExecStart=$bindir/homelab-inventory -addr :$port -dir $state
Environment=AGENT_BIN_DIR=$agents
Restart=on-failure
RestartSec=3
UMask=0077
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=tmpfs
BindPaths=$state
ReadWritePaths=$state
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
CapabilityBoundingSet=$capabilities
AmbientCapabilities=$capabilities
[Install]
WantedBy=multi-user.target
UNIT
  chmod 644 "$units/homelab-inventory.service"
  if [ -z "$prefix" ]; then
    chown homelab-inventory:homelab-inventory "$state"
    systemctl daemon-reload
    systemctl enable --now homelab-inventory.service
    systemctl restart homelab-inventory.service
  fi
  restart='systemctl restart homelab-inventory.service'
fi
printf '%s\n%s\n%s\n' "$mode" "$data" "$port" > "$metadata"
if [ -n "$prefix" ]; then
  printf '\nInstalled disposable binary layout in %s\nStart manually: AGENT_BIN_DIR=%s %s/homelab-inventory -addr 127.0.0.1:%s -dir %s\n' "$prefix" "$agents" "$bindir" "$port" "$state"
  exit 0
fi
healthy=0
deadline=$(($(date +%s) + 30))
while [ "$(date +%s)" -lt "$deadline" ]; do
  if curl -fsS --noproxy '*' --connect-timeout 1 --max-time 1 "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; then healthy=1; break; fi
  if [ "$(date +%s)" -lt "$deadline" ]; then sleep 1; fi
done
[ "$healthy" -eq 1 ] || die 'server did not become healthy; inspect the service and restart'
status=$(curl -fsS --noproxy '*' --connect-timeout 2 --max-time 5 "http://127.0.0.1:$port/setup/status") || die 'cannot read setup status'
setup_state=$(printf '%s' "$status" | sed -n 's/.*"state"[[:space:]]*:[[:space:]]*"\([a-z]*\)".*/\1/p')
left=$(printf '%s' "$status" | sed -n 's/.*"window_seconds_left"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p')
lan=
if command -v ip >/dev/null 2>&1; then lan=$(ip route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<NF;i++) if($i=="src") {print $(i+1); exit}}') || true; fi
if [ -z "$lan" ]; then lan=$(hostname -I 2>/dev/null | awk '{print $1}') || true; fi
lan=${lan:-localhost}
case $lan in *:*) lan="[$lan]";; esac
printf '\nHomelab inventory is ready\n  Open http://%s:%s/\n' "$lan" "$port"
case $setup_state in
  claimed) echo '  Already set up.';;
  unclaimed)
    case $left in ''|*[!0-9]*) die 'invalid setup status';; esac
    printf '  Open within %s minutes; restart with %s to reopen the window.\n' "$(((left + 59) / 60))" "$restart";;
  expired) printf '  Setup window expired; restart with %s to reopen the window.\n' "$restart";;
  *) die 'unknown setup status';;
esac
