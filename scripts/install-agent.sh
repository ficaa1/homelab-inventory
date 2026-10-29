#!/bin/sh
# Run locally on the target Linux host. Served mode downloads verified binaries.
set -eu
umask 077
usage() {
  echo 'Usage: install-agent.sh --api-url URL [--allow-http] [--enroll CODE | --token-file FILE] [--download] [--binary-dir DIR] [--signed --public-key FILE --manifest FILE --artifacts DIR] [--prefix DIR] [--uninstall]'
}
die() { echo "install-agent: $*" >&2; exit 1; }
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_dir=$(dirname "$script_dir")
api_url=${INSTALL_API_URL:-} enroll= download=${INSTALL_DOWNLOAD:-0} token_file= public_key= manifest= artifacts= prefix= signed=0 uninstall=0
allow_http=${ALLOW_HTTP:-0}
case $allow_http in 0|1) ;; *) die 'ALLOW_HTTP must be 0 or 1';; esac
binary_dir=$repo_dir/bin
while [ "$#" -gt 0 ]; do
  case $1 in
    --api-url|--enroll|--token-file|--binary-dir|--public-key|--manifest|--artifacts|--prefix)
      [ "$#" -ge 2 ] || die "missing option value"
      case $1 in
        --api-url) api_url=$2;; --enroll) enroll=$2;; --token-file) token_file=$2;; --binary-dir) binary_dir=$2;;
        --public-key) public_key=$2;; --manifest) manifest=$2;; --artifacts) artifacts=$2;; --prefix) prefix=$2;;
      esac
      shift 2;;
    --allow-http) allow_http=1; shift;;
    --download) download=1; shift;;
    --signed) signed=1; shift;;
    --uninstall) uninstall=1; shift;;
    --help|-h) usage; exit 0;;
    *) usage >&2; die 'unknown option';;
  esac
done
case $prefix in ''|/*) ;; *) die 'prefix must be absolute';; esac
# Paths are also embedded in systemd units; keep their syntax unambiguous.
case $prefix in *[!a-zA-Z0-9/_.-]*) die 'unsupported prefix characters';; /) prefix=;; esac
if [ -z "$prefix" ]; then
  [ "$(id -u)" -eq 0 ] || die 'run as root, or use --prefix for a disposable local test'
  command -v systemctl >/dev/null 2>&1 || die 'systemd required; see docs/install.md for cron'
fi
state=$prefix/var/lib/homelab-agent
bindir=$prefix/usr/local/bin
units=$prefix/etc/systemd/system
if [ "$uninstall" -eq 1 ]; then
  if [ -z "$prefix" ]; then
    systemctl disable --now homelab-discover.timer || true
    systemctl stop homelab-discover.service || true
  fi
  rm -f "$units/homelab-discover.service" "$units/homelab-discover.timer" "$bindir/homelab-discover" "$bindir/discover" "$state/config.json" "$state/token" "$state/.token.new" "$state/agentupdate"
  if [ -z "$prefix" ]; then systemctl daemon-reload; fi
  echo 'install-agent: removed schedule and credentials; retained signed replay state for safe enrollment'
  exit 0
fi
[ "$(uname -s)" = Linux ] || die 'Linux required'
case $(uname -m) in x86_64) arch=amd64;; aarch64|arm64) arch=arm64;; *) die 'Linux amd64 or arm64 required';; esac
api_url=${api_url%/}
[ -n "$api_url" ] || die '--api-url required'
[ -z "$enroll" ] || [ -z "$token_file" ] || die 'choose --enroll or --token-file'
# Validate before any download or redemption. The API origin may not contain
# shell syntax, credentials, paths, queries or fragments; never eval it.
case $api_url in
  https://*) ;;
  http://localhost|http://localhost:*|http://127.0.0.1|http://127.0.0.1:*|http://\[::1\]|http://\[::1\]:*) ;;
  http://*) [ "$allow_http" -eq 1 ] || die 'HTTP exposes credentials; use HTTPS or --allow-http';;
  *) die 'API URL must use http or https';;
esac
origin_host=${api_url#*://}
case $origin_host in ''|*[!a-zA-Z0-9.:[\]-]*) die 'API URL must be an origin with a plain host and optional port';; esac
mkdir -p "$state" "$bindir" "$units"
chmod 700 "$state"
prompt_file= download_dir=
cleanup() {
  if [ -n "$prompt_file" ]; then rm -f "$prompt_file"; fi
  if [ -n "$download_dir" ]; then rm -rf "$download_dir"; fi
  rm -f "$state/.token.new" "$state/.enroll-token"
}
trap cleanup EXIT HUP INT TERM
collector=$binary_dir/discover-linux-$arch
updater=$binary_dir/agentupdate-linux-$arch
if [ "$download" -eq 1 ]; then
  command -v curl >/dev/null 2>&1 || die 'curl required for downloads'
  command -v sha256sum >/dev/null 2>&1 || die 'sha256sum required for verification'
  download_dir=$(mktemp -d "$state/.download-XXXXXX")
  mkdir -p "$download_dir/linux-$arch"
  fetch() {
    curl --fail --silent --show-error --proto '=http,https' --connect-timeout 10 --max-time 120 "$api_url/agent/bin/$1" -o "$2" || die 'agent download failed; check packaged binaries or use manual setup (docs/install.md)'
  }
  fetch SHA256SUMS "$download_dir/SHA256SUMS"
  for name in discover agentupdate; do
    fetch "linux-$arch/$name" "$download_dir/linux-$arch/$name"
    # Check only the two exact paths, never arbitrary entries from the server.
    expected=$(awk -v path="linux-$arch/$name" '$2 == path && length($1) == 64 && $1 !~ /[^0123456789abcdef]/ {print $1}' "$download_dir/SHA256SUMS")
    actual=$(sha256sum "$download_dir/linux-$arch/$name")
    actual=${actual%% *}
    [ -n "$expected" ] && [ "$actual" = "$expected" ] || die 'agent checksum mismatch'
    chmod 755 "$download_dir/linux-$arch/$name"
  done
  collector=$download_dir/linux-$arch/discover
  updater=$download_dir/linux-$arch/agentupdate
fi
[ -x "$collector" ] && [ -x "$updater" ] || die 'build artifacts first with make dist, supply --binary-dir or use --download'
export ALLOW_HTTP=$allow_http
if [ -n "$enroll" ]; then
  "$updater" -enroll "$enroll" -api-url "$api_url" -token-file "$state/.enroll-token"
  token_file=$state/.enroll-token
fi
if [ -z "$token_file" ]; then
  [ -t 0 ] || die '--token-file required without a terminal'
  prompt_file=$(mktemp "$state/.token-XXXXXX")
  printf 'API token (hidden): ' >&2
  old_stty=$(stty -g)
  trap 'stty "$old_stty"; cleanup' EXIT HUP INT TERM
  stty -echo
  IFS= read -r token || { stty "$old_stty"; die 'token input failed'; }
  stty "$old_stty"
  printf '\n' >&2
  printf '%s\n' "$token" > "$prompt_file"
  unset token
  token_file=$prompt_file
fi
[ -f "$token_file" ] || die 'token file is not a regular file'
# Validate before replacing working credentials; nothing is posted yet.
export ALLOW_HTTP=$allow_http
"$updater" -check-config -api-url "$api_url" -token-file "$token_file" -dir "$state" -discover "$collector"
if [ "$signed" -eq 1 ]; then
  [ -n "$public_key" ] && [ -f "$public_key" ] || die '--signed requires --public-key'
  if [ ! -f "$state/state.json" ]; then
    [ -n "$manifest" ] && [ -f "$manifest" ] && [ -d "$artifacts" ] || die 'first signed enrollment requires --manifest and --artifacts'
  fi
fi
# Finish any running report before replacing its pinned runner/configuration.
if [ -z "$prefix" ]; then
  if [ -f "$units/homelab-discover.timer" ]; then systemctl stop homelab-discover.timer; fi
  if [ -f "$units/homelab-discover.service" ]; then systemctl stop homelab-discover.service; fi
fi
cp "$token_file" "$state/.token.new"
chmod 600 "$state/.token.new"
install_binary() {
  cp "$1" "$2.new"
  chmod 755 "$2.new"
  mv -f "$2.new" "$2"
}
install_binary "$updater" "$state/agentupdate"
if [ "$signed" -eq 1 ]; then
  [ -n "$public_key" ] && [ -f "$public_key" ] || die '--signed requires --public-key'
  if [ ! -f "$state/state.json" ]; then
    [ -n "$manifest" ] && [ -n "$artifacts" ] || die 'first signed enrollment requires --manifest and --artifacts'
    cp "$public_key" "$state/public.key"
    chmod 600 "$state/public.key"
    "$state/agentupdate" -dir "$state" -public "$state/public.key" -initialize "$manifest" -input "$artifacts"
  else
    cmp -s "$public_key" "$state/public.key" || die 'key differs; follow the explicit key rotation procedure'
  fi
fi
if [ -f "$state/state.json" ]; then
  signed=1
  # Preserve signed replay state on an idempotent installer rerun.
  ln -sf "$state/current/discover" "$bindir/discover"
else
  install_binary "$collector" "$bindir/discover"
fi
mv -f "$state/.token.new" "$state/token"
# Configuration contains paths, never shell code. Upgrades change only this mode.
if [ "$signed" -eq 1 ]; then
  "$state/agentupdate" -configure -config "$state/config.json" -dir "$state" -discover "$bindir/discover" -public "$state/public.key" -api-url "$api_url" -token-file "$state/token" -signed
else
  "$state/agentupdate" -configure -config "$state/config.json" -dir "$state" -discover "$bindir/discover" -api-url "$api_url" -token-file "$state/token"
fi
cat > "$bindir/homelab-discover.new" <<WRAPPER
#!/bin/sh
exec "$state/agentupdate" -config "$state/config.json"
WRAPPER
chmod 755 "$bindir/homelab-discover.new"
mv -f "$bindir/homelab-discover.new" "$bindir/homelab-discover"
cat > "$units/homelab-discover.service" <<UNIT
[Unit]
Description=Collect and post host inventory
Wants=network-online.target
After=network-online.target
[Service]
Type=oneshot
ExecStart=$bindir/homelab-discover
TimeoutStartSec=15min
UMask=0077
UNIT
chmod 644 "$units/homelab-discover.service"
cat > "$units/homelab-discover.timer" <<'TIMER'
[Unit]
Description=Run homelab-discover.service on a schedule
[Timer]
OnBootSec=2min
OnUnitActiveSec=15min
Persistent=true
[Install]
WantedBy=timers.target
TIMER
chmod 644 "$units/homelab-discover.timer"
if [ -z "$prefix" ]; then
  systemctl daemon-reload
  systemctl enable --now homelab-discover.timer
  systemctl start homelab-discover.service
else
  "$bindir/homelab-discover"
fi
echo 'install-agent: installed; first report completed'
