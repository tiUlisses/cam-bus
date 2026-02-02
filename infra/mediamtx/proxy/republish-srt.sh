#!/bin/sh
set -eu

proxy_url=""
candidate_list=""

add_candidates() {
  if [ "$#" -eq 0 ]; then
    return 0
  fi
  if [ -z "$candidate_list" ]; then
    candidate_list="$*"
    return 0
  fi
  candidate_list="$candidate_list $*"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --proxy-url)
      shift
      proxy_url="${1:-}"
      ;;
    --candidate|--srt)
      shift
      add_candidates "${1:-}"
      ;;
    --)
      shift
      add_candidates "$@"
      break
      ;;
    *)
      add_candidates "$1"
      ;;
  esac
  shift || true
done

if [ -z "$candidate_list" ] && [ -n "${UPLINK_SRT_CANDIDATES:-}" ]; then
  candidate_list="$(printf '%s' "$UPLINK_SRT_CANDIDATES" | tr ',;' ' ')"
fi

if [ -z "$proxy_url" ]; then
  echo "[uplink] missing --proxy-url"
  exit 1
fi

if [ -z "$candidate_list" ]; then
  echo "[uplink] no SRT candidates provided"
  exit 1
fi

backoff="${UPLINK_SRT_RETRY_BACKOFF_SECONDS:-2}"
case "$backoff" in
  ""|*[!0-9]*)
    backoff=2
    ;;
esac

global_args="${UPLINK_FFMPEG_GLOBAL_ARGS:--hide_banner}"
input_args="${UPLINK_FFMPEG_INPUT_ARGS:--rtsp_transport tcp}"
output_args="${UPLINK_FFMPEG_OUTPUT_ARGS:--c copy -f mpegts -mpegts_flags +resend_headers -muxdelay 0 -muxpreload 0}"

for candidate in $candidate_list; do
  echo "[uplink] trying SRT candidate: $candidate"
  if ffmpeg $global_args $input_args -i "$proxy_url" $output_args "$candidate"; then
    exit 0
  else
    status=$?
  fi
  case "$status" in
    ""|*[!0-9]*)
      status=127
      ;;
  esac
  echo "[uplink] SRT candidate failed (exit $status): $candidate"
  sleep "$backoff"
done

echo "[uplink] all SRT candidates failed"
exit 1
