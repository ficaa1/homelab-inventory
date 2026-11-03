#!/bin/sh
# Run locally on the target Linux host. Served mode downloads verified binaries.
set -eu
umask 077
usage() {
  echo 'Usage: install-agent.sh --api-url URL [--allow-http] [--enroll CODE | --token-file FILE] [--download] [--binary-dir DIR] [--updates slots] [--scheduler truenas-cron --state-dir DIR [--replace-schedule ID]] [--signed --public-key FILE --manifest FILE --artifacts DIR] [--prefix DIR] [--uninstall]'
}
die() { echo "install-agent: $*" >&2; exit 1; }
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_dir=$(dirname "$script_dir")
api_url=${INSTALL_API_URL:-} enroll= download=${INSTALL_DOWNLOAD:-0} token_file= public_key= manifest= artifacts= prefix= signed=0 uninstall=0
expected_host=
updates=legacy scheduler=systemd state_dir= replace_schedule=
allow_http=${ALLOW_HTTP:-0}
case $allow_http in 0|1) ;; *) die 'ALLOW_HTTP must be 0 or 1';; esac
binary_dir=$repo_dir/bin
while [ "$#" -gt 0 ]; do
  case $1 in
    --api-url|--enroll|--token-file|--binary-dir|--public-key|--manifest|--artifacts|--prefix|--updates|--scheduler|--state-dir|--replace-schedule|--hostname)
      [ "$#" -ge 2 ] || die "missing option value"
      case $1 in
        --api-url) api_url=$2;; --enroll) enroll=$2;; --token-file) token_file=$2;; --binary-dir) binary_dir=$2;;
        --public-key) public_key=$2;; --manifest) manifest=$2;; --artifacts) artifacts=$2;; --prefix) prefix=$2;; --updates) updates=$2;; --hostname) expected_host=$2;; --scheduler) scheduler=$2;; --state-dir) state_dir=$2;; --replace-schedule) replace_schedule=$2;;
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
case $updates in legacy|slots) ;; *) die "--updates must be slots (or omit it for legacy mode)";; esac
case $prefix in ''|/*) ;; *) die 'prefix must be absolute';; esac
# Paths are also embedded in systemd units; keep their syntax unambiguous.
case $prefix in *[!a-zA-Z0-9/_.-]*) die 'unsupported prefix characters';; /) prefix=;; esac
case $scheduler in systemd|truenas-cron) ;; *) die '--scheduler must be systemd or truenas-cron';; esac
if [ "$scheduler" = systemd ] && command -v midclt >/dev/null 2>&1 && { grep -qi truenas /etc/version 2>/dev/null || [ -e /usr/bin/middlewared ]; }; then
  die 'TrueNAS requires --scheduler truenas-cron with a dedicated --state-dir'
fi
if [ -z "$prefix" ]; then
  [ "$(id -u)" -eq 0 ] || die 'run as root, or use --prefix for a disposable local test'
  if [ "$scheduler" = systemd ]; then command -v systemctl >/dev/null 2>&1 || die 'systemd required; use --scheduler truenas-cron on TrueNAS'; fi
fi
state=$prefix/var/lib/homelab-agent
if [ "$scheduler" = truenas-cron ]; then
  [ "$updates" = slots ] || die 'truenas-cron requires --updates slots'
  [ -n "$state_dir" ] || die 'truenas-cron requires --state-dir on a mounted executable dataset'
  [ -z "$prefix" ] || die 'truenas-cron uses --state-dir, not --prefix'
  state=$state_dir
elif [ -n "$state_dir" ]; then die '--state-dir requires --scheduler truenas-cron'; fi
bindir=$prefix/usr/local/bin
units=$prefix/etc/systemd/system
if [ "$uninstall" -eq 1 ]; then
  [ "$scheduler" = systemd ] || die 'disable the config-DB cron job explicitly; dataset replay state is retained'
  if [ -z "$prefix" ]; then
    systemctl disable --now homelab-discover.timer || true
    systemctl stop homelab-discover.service || true
  fi
  rm -f "$units/homelab-discover.service" "$units/homelab-discover.timer" "$bindir/homelab-discover" "$bindir/discover" "$state/config.json" "$state/token" "$state/.token.new" "$state/agentupdate" "$state/agentboot"
  # A completed migration backup also contains the previous credential.
  if [ -d "$state/.migration" ]; then
    find "$state/.migration" -type d -exec chmod 700 {} \;
    rm -rf "$state/.migration"
  fi
  if [ -z "$prefix" ]; then systemctl daemon-reload; fi
  echo 'install-agent: removed schedule and credentials; retained signed replay state for safe enrollment'
  exit 0
fi
if [ "$updates" = legacy ] && { [ -f "$state/control.0" ] || [ -f "$state/control.1" ]; }; then
  die 'existing slots installation requires --updates slots; refusing to bypass its floor'
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
# The slots coordinator owns mount validation, durable preparation and cutover.
# Download only its bootstrap executable here; it re-downloads the installed
# launcher and all trust/runtime files over the authenticated agent channel.
if [ "$updates" = slots ]; then
  launcher=$binary_dir/agentboot-linux-$arch
  bootstrap_dir=
  if [ "$download" -eq 1 ]; then
    command -v curl >/dev/null 2>&1 || die 'curl required for downloads'
    command -v sha256sum >/dev/null 2>&1 || die 'sha256sum required for verification'
    # TrueNAS needs the bootstrap file on its dataset too. Validate the effective
    # mount before mkdir: refuse an unmounted pool's empty mountpoint.
    if [ "$scheduler" = truenas-cron ]; then
      case $state in /mnt/*) ;; *) die 'choose a mounted dataset directory under /mnt';; esac
      case $state in *[!a-zA-Z0-9/_.-]*|*/../*|*/..|*/./*|*/.|*//*|*/) die 'state directory must be an absolute plain path without traversal';; esac
      probe=$state
      while [ ! -d "$probe" ]; do probe=${probe%/*}; [ -n "$probe" ] || die 'dataset is not mounted'; done
      mount_info=$(findmnt -n -T "$probe" -o TARGET,FSTYPE,OPTIONS) || die 'dataset mount cannot be read'
      set -- $mount_info
      [ "$#" -eq 3 ] && [ "$1" != / ] && [ "$2" = zfs ] || die 'state directory must be on a mounted ZFS dataset'
      case ,$3, in *,noexec,*|*,ro,*) die 'dataset must be writable with exec allowed';; esac
      [ "$state" != "$1" ] || die 'state directory must be a dedicated subdirectory, not a pool or dataset mountpoint'
      case $state/ in "$1"/*) ;; *) die 'state directory is outside mounted dataset';; esac
      # Only datasets below the covering mount matter: TrueNAS keeps inactive
      # boot environments as unmounted datasets mounted at /.
      zfs_mounts=$(zfs list -H -o name,mountpoint,mounted) || die 'cannot inspect configured ZFS dataset mountpoints'
      printf '%s\n' "$zfs_mounts" | awk -F '\t' -v path="$state" -v mnt="$1" '
        NF != 3 { exit 1 }
        $2 ~ /^\// && $3 != "yes" && index($2, mnt "/") == 1 && (path == $2 || index(path, $2 "/") == 1) { exit 1 }
      ' || die 'state directory is at or below an unmounted ZFS dataset; mount it first or choose a new path'
    fi
    if [ "$scheduler" = truenas-cron ]; then bootstrap_parent=$1; fi
    # Refuse symlink ancestors before touching directory permissions.
    probe=$state
    while [ "$probe" != / ]; do
      [ ! -L "$probe" ] || die 'state path must contain only real directories'
      probe=${probe%/*}; [ -n "$probe" ] || probe=/
    done
    if [ "$scheduler" = truenas-cron ]; then
      # Leave state untouched for Go's authoritative ownership/marker check.
      # Execute bootstrap on the already validated dataset, even if /tmp is noexec.
      bootstrap_dir=$(mktemp -d "$bootstrap_parent/.bootstrap-XXXXXX")
    else
      mkdir -p "$state"
      chmod 700 "$state"
      bootstrap_dir=$(mktemp -d "$state/.bootstrap-XXXXXX")
    fi
    trap 'rm -rf "$bootstrap_dir"' 0
    trap 'exit 1' HUP INT TERM
    for file in SHA256SUMS linux-$arch/agentboot; do
      name=${file##*/}
      curl --fail --silent --show-error --proto '=http,https' --connect-timeout 10 --max-time 120 "$api_url/agent/bin/$file" -o "$bootstrap_dir/$name" || die 'launcher bootstrap download failed'
    done
    expected=$(awk -v path="linux-$arch/agentboot" '$2 == path && length($1) == 64 && $1 !~ /[^0123456789abcdef]/ {print $1}' "$bootstrap_dir/SHA256SUMS")
    actual=$(sha256sum "$bootstrap_dir/agentboot"); actual=${actual%% *}
    [ -n "$expected" ] && [ "$expected" = "$actual" ] || die 'launcher bootstrap checksum mismatch'
    chmod 755 "$bootstrap_dir/agentboot"
    launcher=$bootstrap_dir/agentboot
  fi
  [ -x "$launcher" ] || die 'agentboot required; make dist or use --download'
  set -- -install -hostname "$expected_host" -dir "$state" -prefix "$prefix" -scheduler "$scheduler" -api-url "$api_url"
  [ "$allow_http" -eq 0 ] || set -- "$@" -allow-http
  [ -z "$enroll" ] || set -- "$@" -enroll "$enroll"
  [ -z "$token_file" ] || set -- "$@" -token-file "$token_file"
  [ -z "$public_key" ] || set -- "$@" -public "$public_key"
  [ -z "$manifest" ] || set -- "$@" -initialize "$manifest"
  [ -z "$artifacts" ] || set -- "$@" -input "$artifacts"
  [ -z "$replace_schedule" ] || set -- "$@" -replace-schedule "$replace_schedule"
  "$launcher" "$@"
  echo 'install-agent: signed slots installed; first report accepted'
  exit 0
fi
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
