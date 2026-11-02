#!/bin/bash
# Park GPU workers and send one Apprise notification when a batch drains.
set -euo pipefail
set -a
# shellcheck disable=SC1091
[ ! -f /etc/default/tdarr-shrink ] || . /etc/default/tdarr-shrink
set +a
TDARR_API="${TDARR_API:-http://127.0.0.1:8266}"
STATE_DIR="${TDARR_STATE_DIR:-/var/lib/tdarr-shrink}"
STATE_FILE="${STATE_DIR}/state"
LOG_FILE="${TDARR_LOG_FILE:-/var/log/tdarr-shrink.log}"
CURL_AUTH=()
[ -z "${TDARR_API_KEY:-}" ] || CURL_AUTH=(-H "x-api-key: ${TDARR_API_KEY}")

log() { printf "%s %s\n" "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*" >>"${LOG_FILE}"; }
api() {
  curl -fsS "${CURL_AUTH[@]}" --max-time 20 -X "$1" "${TDARR_API}$2" \
    -H 'Content-Type: application/json' "${@:3}"
}

mkdir -p "${STATE_DIR}"
exec 9>"${STATE_DIR}/lock"
flock -n 9 || exit 0
[ -f "${STATE_FILE}" ] || printf 'idle\n' >"${STATE_FILE}"
state=$(tr -d ' \n' <"${STATE_FILE}")
case "${state}" in busy|idle) ;; *) state=idle ;; esac

workers=$(api GET /api/v2/get-nodes 2>/dev/null) || { log 'Tdarr unavailable; retry next cycle'; exit 0; }
files=$(api POST /api/v2/cruddb -d '{"data":{"collection":"FileJSONDB","mode":"getAll"}}' 2>/dev/null) \
  || { log 'File database unavailable; retry next cycle'; exit 0; }
# Unknown responses must never look like an empty queue.
printf '%s' "${workers}" | jq -e 'type == "object" and length > 0' >/dev/null || exit 1
printf '%s' "${files}" | jq -e 'type == "array" and all(.[]; has("TranscodeDecisionMaker"))' >/dev/null || exit 1
if printf '%s' "${workers}" | jq -e '[.. | objects | select(.idle? == false)] | length > 0' >/dev/null \
  || printf '%s' "${files}" | jq -e 'any(.[]; .TranscodeDecisionMaker == "Queued" or .TranscodeDecisionMaker == "Transcoding")' >/dev/null; then
  printf 'busy\n' >"${STATE_FILE}"
  exit 0
fi
[ "${state}" = busy ] || exit 0

# A failed park leaves state busy and retries; do not announce completion first.
for id in $(printf '%s' "${workers}" | jq -r 'keys[]'); do
  cur=$(printf '%s' "${workers}" | jq -r --arg id "${id}" '.[$id].workerLimits.transcodegpu // 0')
  while [ "${cur}" -gt 0 ]; do
    api POST /api/v2/alter-worker-limit -d "$(jq -nc --arg id "${id}" \
      '{data:{nodeID:$id,process:"decrease",workerType:"transcodegpu"}}')" >/dev/null
    cur=$((cur - 1))
  done
done
libs=$(api POST /api/v2/cruddb -d '{"data":{"collection":"LibrarySettingsJSONDB","mode":"getAll"}}')
for lib in $(printf '%s' "${libs}" | jq -r --arg m "${TDARR_MOVIE_ROOT:-/media/movies}" \
  --arg s "${TDARR_SERIES_ROOT:-/media/series}" '.[] | select(.folder == $m or .folder == $s) | ._id'); do
  api POST /api/v2/cruddb -d "$(jq -nc --arg lib "${lib}" \
    '{data:{collection:"LibrarySettingsJSONDB",mode:"update",docID:$lib,obj:{processLibrary:false,processTranscodes:false}}}')" >/dev/null
done
review=$(printf '%s' "${files}" | jq '[.[] | select(.TranscodeDecisionMaker != "Not required" and .TranscodeDecisionMaker != "Transcode success")] | length')
type=success
body='Tdarr batch done'
if [ "${review}" -gt 0 ]; then
  type=warning
  body="Tdarr batch done — ${review} files need review"
fi
if [ -z "${APPRISE_NOTIFY:-}" ]; then
  log 'APPRISE_NOTIFY is unset; workers parked, notification pending'
  exit 0
fi
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 30 -X POST "${APPRISE_NOTIFY}" \
  --data-urlencode "body=${body}" --data-urlencode 'title=Tdarr' --data-urlencode "type=${type}" || true)
case "${code}" in
  200|201|202|203|205|206|207|208|226)
    printf 'idle\n' >"${STATE_FILE}"
    log "batch drained; notification accepted (${type})" ;;
  *) log "notification failed (HTTP ${code}); retry next cycle" ;;
esac
