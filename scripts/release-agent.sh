#!/bin/sh
# Run on a trusted signer, or use a preinstalled signer over optional SSH.
set -eu
command_name=${1:-}
[ "$#" -eq 0 ] || shift
SIGNER_BIN=${SIGNER_BIN:-./bin/agentsign}
LOCAL_SIGNER_BIN=${LOCAL_SIGNER_BIN:-./bin/agentsign}
SIGNER_KEY=${SIGNER_KEY:-$HOME/.config/homelab-inventory/release.key}
SIGNER_DIR=${SIGNER_DIR:-$HOME/inventory-releases}
SIGNER_INPUT=${SIGNER_INPUT:-bin}
SIGNER_PORT=${SIGNER_PORT:-22}
AGENT_PLATFORMS=${AGENT_PLATFORMS:-linux/amd64,linux/arm64}
export AGENT_PLATFORMS
case $command_name in
 keygen) set -- -keygen;;
 release) [ "$#" -eq 1 ] || { echo 'release requires VERSION' >&2; exit 1; }; set -- -input "$SIGNER_INPUT" -version "$1";;
 hold|promote|auto) set -- -control "$command_name";;
 resign) set -- -resign;;
 *) echo 'usage: release-agent.sh keygen|release VERSION|hold|promote|auto|resign' >&2; exit 1;;
esac
if [ -z "${SIGNER_HOST:-}" ]; then
 exec "$SIGNER_BIN" -key "$SIGNER_KEY" -dir "$SIGNER_DIR" "$@"
fi
# SSH paths are explicit and use conservative characters to avoid remote shell
# interpretation. Preinstall a trusted native signer on that machine.
for value in "$SIGNER_HOST" "$SIGNER_PORT" "$SIGNER_BIN" "$SIGNER_KEY" "$SIGNER_DIR" "$AGENT_PLATFORMS" "$@"; do
 case $value in ''|*[!a-zA-Z0-9/._:@,=-]*) echo 'unsupported remote option characters' >&2; exit 1;; esac
done
case $SIGNER_HOST in -*) echo 'invalid signer host' >&2; exit 1;; esac
remote_input=
cleanup() {
 if [ -n "$remote_input" ]; then ssh -p "$SIGNER_PORT" "$SIGNER_HOST" "rm -rf -- '$remote_input'"; fi
}
trap cleanup EXIT HUP INT TERM
if [ "$command_name" = release ]; then
 version=$4
 remote_input=$(ssh -p "$SIGNER_PORT" "$SIGNER_HOST" 'mktemp -d /tmp/inventory-release.XXXXXXXX')
 case $remote_input in /tmp/inventory-release.*) ;; *) exit 1;; esac
 case $remote_input in *[!a-zA-Z0-9/._-]*) exit 1;; esac
 # The local signer parses exactly the same contract as the build and manifest.
 for platform in $("$LOCAL_SIGNER_BIN" -platforms); do
  scp -P "$SIGNER_PORT" "$SIGNER_INPUT/discover-$platform" "$SIGNER_HOST:$remote_input/"
 done
 set -- -input "$remote_input" -version "$version"
fi
remote_command="AGENT_PLATFORMS='$AGENT_PLATFORMS' '$SIGNER_BIN' -key '$SIGNER_KEY' -dir '$SIGNER_DIR'"
for arg do remote_command="$remote_command '$arg'"; done
ssh -p "$SIGNER_PORT" "$SIGNER_HOST" "$remote_command"
