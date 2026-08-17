#!/usr/bin/env bash
# Side-by-side growing-file test for Premiere.
#
# Writes the SAME live picture into two growing files at once:
#
#   growtest_prores.mov   ProRes HQ in QuickTime  — what Live Record writes today
#   growtest_avci.mxf     AVC-Intra 100 in MXF    — on Adobe's supported growing list
#
# Import BOTH into Premiere while they are still being written, and compare:
#
#   * Does the clip name appear in ITALICS in the Project panel?
#   * Does the duration grow on its own every 10s, without tabbing away?
#   * Does the Source Monitor's Force Media Refresh button do anything?
#
# The picture carries a burnt-in wall clock, so the gap between what the clip
# shows and the real time tells you how far behind each one is running.
#
#   ./growtest.sh [seconds]      default 900 (15 minutes)
#   ./growtest.sh stop
set -uo pipefail
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"

OUT="${GROWTEST_DIR:-$HOME/Movies/Live Record}"
SECS="${1:-900}"

if [[ "${1:-}" == "stop" ]]; then
  pkill -f "growtest_" && echo "stopped" || echo "nothing running"
  exit 0
fi

mkdir -p "$OUT"
rm -f "$OUT/growtest_prores.mov" "$OUT/growtest_avci.mxf"

echo "Writing two growing files for $SECS seconds into:"
echo "  $OUT"
echo

# ---- ProRes HQ / QuickTime, exactly the muxer settings Live Record uses ----
gst-launch-1.0 -e -q \
  videotestsrc pattern=smpte is-live=true \
  ! video/x-raw,width=1920,height=1080,framerate=25/1 \
  ! clockoverlay font-desc="Sans Bold 48" halignment=center valignment=center \
  ! timeoverlay font-desc="Sans 30" \
  ! vtenc_prores ! "video/x-prores,variant=(string)hq" ! queue ! mux.video_0 \
  audiotestsrc is-live=true wave=sine freq=440 volume=0.3 \
  ! audioconvert ! "audio/x-raw,rate=48000,channels=2,format=S24BE" ! queue ! mux.audio_0 \
  qtmux name=mux reserved-max-duration=21600000000000 \
    reserved-moov-update-period=1000000000 reserved-bytes-per-sec=700 \
    force-create-timecode-trak=true \
  ! filesink location="$OUT/growtest_prores.mov" sync=false \
  > /tmp/growtest_prores.log 2>&1 &
echo "  growtest_prores.mov   ProRes HQ / QuickTime   (pid $!)"

# ---- AVC-Intra 100 / MXF OP1a, via ffmpeg (GStreamer's mxfmux cannot grow) ----
# -re paces the synthetic source to real time, as a live feed would arrive.
# testsrc (not testsrc2) burns in a running frame counter and timestamp, so the
# clip shows how far it has got. drawtext is not available: this ffmpeg is built
# without libfreetype.
ffmpeg -hide_banner -loglevel error -re \
  -f lavfi -i "testsrc=size=1920x1080:rate=25" \
  -re -f lavfi -i "sine=frequency=440:sample_rate=48000" \
  -c:v libx264 -avcintra-class 100 -pix_fmt yuv422p10le \
  -c:a pcm_s24le -ar 48000 -ac 2 \
  -f mxf -t "$SECS" "$OUT/growtest_avci.mxf" \
  > /tmp/growtest_avci.log 2>&1 &
echo "  growtest_avci.mxf     AVC-Intra 100 / MXF     (pid $!)"

echo
echo "Import both into Premiere NOW, while they are still growing."
echo "Watch for: italics in the Project panel, self-updating duration, and"
echo "whether Force Media Refresh does anything."
echo
echo "Stop early with: ./growtest.sh stop"

sleep "$SECS"
pkill -f "growtest_" 2>/dev/null
echo "done"
