#!/usr/bin/env bash
# Local SRT test sources for developing against Live Record.
#
# Starts one SRT *listener* per feed sending 1080p25 H.264 + AAC over MPEG-TS,
# which is what a real contribution encoder sends. Live Record dials into these
# as the caller, exactly as it will dial a real encoder.
#
#   ./testsrc.sh          two sources on 9001 and 9002
#   ./testsrc.sh 9001     just one
#   ./testsrc.sh stop     stop them all

set -uo pipefail
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"

if [[ "${1:-}" == "stop" ]]; then
  pkill -f "srtsink uri=srt://" && echo "test sources stopped" || echo "none running"
  exit 0
fi

PORTS=("${@:-9001 9002}")
read -ra PORTS <<< "${PORTS[*]}"

for i in "${!PORTS[@]}"; do
  port="${PORTS[$i]}"
  # A distinct pattern and audio tone per source makes it obvious in the UI
  # which preview belongs to which feed.
  case $((i % 2)) in
    0) pattern=ball;  freq=440;  vol=0.2; label="TEST-A" ;;
    1) pattern=smpte; freq=880;  vol=0.5; label="TEST-B" ;;
  esac

  # timeoverlay's text= prefix is used instead of a separate textoverlay: a
  # textoverlay with an unlinked text sink pad stalls a live pipeline, which
  # looks exactly like a dead SRT feed.
  gst-launch-1.0 -q \
    videotestsrc pattern=$pattern is-live=true \
    ! video/x-raw,width=1920,height=1080,framerate=25/1 \
    ! timeoverlay text="$label" font-desc="Sans 36" \
    ! clockoverlay halignment=right font-desc="Sans 36" \
    ! x264enc tune=zerolatency bitrate=8000 key-int-max=50 speed-preset=veryfast \
    ! h264parse config-interval=-1 \
    ! queue max-size-time=0 max-size-bytes=0 max-size-buffers=0 \
    ! mpegtsmux name=m alignment=7 \
    ! srtsink uri="srt://:$port?mode=listener" wait-for-connection=false latency=200 \
    audiotestsrc is-live=true wave=sine freq=$freq volume=$vol \
    ! audioconvert ! avenc_aac bitrate=128000 ! aacparse \
    ! queue max-size-time=0 max-size-bytes=0 max-size-buffers=0 ! m. \
    >/dev/null 2>&1 &

  echo "  $label  srt://127.0.0.1:$port  (listener, pid $!)"
done

echo
echo "Sources running. Stop with: ./testsrc.sh stop"
wait
