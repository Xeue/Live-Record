# Where we got to — 15 Aug 2026

## Signed

`./sign.sh` builds and signs with **Developer ID Application: Sygnal TV Ltd
(5P76UVY5WF)**, secure timestamp, bundle ID `tv.sygnal.liverecord`. Verified:
valid on disk, satisfies its Designated Requirement.

`./sign.sh notarize` submits and staples, once credentials are stored:

    xcrun notarytool store-credentials liverecord \
      --apple-id you@example.com --team-id 5P76UVY5WF --password <app-specific-password>

**The hardened runtime is ON** (`flags=0x10000(runtime)`), with all five
entitlements from `build/darwin/entitlements.plist` embedded and verified in the
signed binary. They are not optional: without them GStreamer's ORC JIT cannot map
write+execute memory and every pipeline stalls silently, writing zero bytes —

    ORC: ERROR: Failed to create write and exec mmap regions.

Verified as a positive control: zero "ORC: ERROR" in a signed 60-second run that
wrote 3.9 GB across four files.

**Not yet notarised.** Gatekeeper reports "Unnotarized Developer ID", so a copy
taken to another Mac will be blocked until `./sign.sh notarize` has been run.

## FIXED — the app records both formats on both feeds

Both formats on both feeds now work in the .app, composited preview and all.
Measured, 60 seconds, `previewMode: native`:

| file | video | audio | frames |
|---|---|---|---|
| CAM-A .mov | 59.57s | 59.63s | 1490 |
| CAM-A .mxf | 59.60s | 59.64s | 1490 |
| CAM-B .mov | 59.72s | 60.14s | 1493 |
| CAM-B .mxf | 59.72s | 60.16s | 1493 |

Zero watchdog restarts, one window owned by the app (so glimagesink took the
supplied view), and the MXF frame counts match the ProRes masters exactly — not
one frame dropped. The CLI is unchanged in behaviour and still good.

### It was never an in-process bug

**The pipeline could not leave PAUSED.** A `GstBaseSink` finishes READY→PAUSED
only once it has been handed a buffer, and a `GstBin` keeps every child in
PAUSED until all of its async children are done. So the pipeline only reaches
PLAYING after *all three* of its sinks have seen data — and with both formats
enabled they cannot:

- mxfmux's filesink needs x264enc's first frame, and x264enc holds a dozen-odd
  frames before it emits anything;
- meanwhile the ProRes branch's queues fill, because *its* filesink has already
  prerolled and is parked in `gst_base_sink_wait_preroll` waiting for a PLAYING
  that cannot arrive;
- a full queue back-pressures the tee, so the decoder stops and x264enc never
  gets the frames it needs.

On top of that the decode thread wedges permanently: `GST_QUERY_ALLOCATION` is a
SERIALIZED query, so vtdec's pool negotiation is queued behind the stuck buffers
in the preview queue and never returns.

Confirmed with `GST_DEBUG=GST_STATES:5` (`<sink_CAMB1> current READY pending
PAUSED, desired next PLAYING` — the sinks are handed a target they can never
act on) and a thread sample of the wedged process (two sinks in
`gst_base_sink_wait_preroll`, qtmux blocked pushing its ftyp into one of them).

The reason it looked like an in-process-only bug is that it is a race between
how long the slowest sink takes to preroll and how much the other branches can
buffer meanwhile — and **glimagesink's GL context takes ~1.2s to come up**,
against microseconds for the fdsink the CLI's browser preview uses. Same
pipeline, same latent bug, different odds. Under `GST_DEBUG` both feeds lost.

**Fix:** `async=false` on every sink. Preroll buys a live recorder nothing —
there is no frame to show while PAUSED, because a live source produces nothing
until PLAYING.

### Two more bugs found on the way

- **`leaky=downstream` on the MXF branch could abort the whole app.** mxfmux
  asserts `index_pos_diff < 127 && index_pos_diff >= -127` where
  `index_pos_diff = pts_pos - pad->pos`; a dropped buffer widens that gap
  permanently, so the 128th drop kills the process — both feeds, mid-take,
  neither file finalised. Measured: a run whose MXFs came out 106 and 196 frames
  short died exactly there, `Bail out! ERROR: mxfmux.c:1384`. Those queues are
  no longer leaky. They were only made leaky for the tee-backpressure theory,
  which was already disproven.
- **The in-process engine closed the fd `fdsink` was writing to.** In a child
  process the write end is a copy and closing ours is right; in-process it is
  the sink's own descriptor. `previewMode` of `browser` or `both` died within a
  second with "Bad file descriptor". It is closed at teardown now, after the
  pipeline is at NULL.
- **Watchdog restarts deadlocked the in-process engine.** `stopFn` ends with
  `<-runDone`, and the watchdog calls it from `pollProgress` — a goroutine that
  `wg.Wait()` is waiting on. `runDone` is closed before `wg.Wait()` now, as the
  subprocess engine always did.

`pipeline_test.go` locks in the two pipeline invariants: every sink carries
`async=false`, and nothing on a path into mxfmux is leaky. Both fail if the
respective fix is reverted.

---
# Where we got to — 14 Aug 2026 (session 4) — the composited app is DONE

```sh
open build/bin/liverecord.app
```

All three phases finished and verified running:

1. **Wails shell** — native window, native folder picker, settings in
   Application Support, loopback listener for the browser preview.
2. **In-process GStreamer** — `gst_parse_launch` on the same description the
   subprocess engine uses, typed bus reads, and EOS-then-wait finalisation
   replacing gst-launch's `-e`.
3. **Composited preview** — an NSView added as a sibling of the WKWebView,
   handed to `glimagesink` through `GstVideoOverlay`. The page reports each
   feed's video rectangle on every layout and resize, and the surface follows.

Measured on the last clean run: both feeds up in-process within a second of each
other, audio meters live, identical time-of-day timecode `20:57:03:14` on both
files, audio tracking video (12.44s/14.05s and 12.48s/14.35s), and **one window
owned by the app** — proof the sink took our view instead of opening its own.

The command-line build is unchanged and still uses crash-isolated subprocesses;
verified side by side in the same session, same results.

Two bugs found while wiring it up:

- `level` posts its RMS as a GLib `GValueArray`, not GStreamer's own array type.
  Checking only for `GST_TYPE_ARRAY` gave messages with no values and meters
  pinned at the floor — the typed equivalent of the regex bug from session 2.
  The test now asserts real RMS values, not just message counts.
- A `.app` in a user folder is writable, so the writability test dropped
  `config.json` inside `Contents/MacOS` — invisible, wiped by each build, and
  enough to break the signature. Bundle detection is by path now.

## Still open

- **Premiere verification** — still needs your eyes. Nothing about it changed.
- Bundling GStreamer into the .app for redistribution. Today it uses the
  Homebrew install (`findGst` already falls back to `/opt/homebrew/bin`, which
  is what a Finder-launched app needs since it inherits a minimal PATH). The
  recipe is `build/bundle-gst-darwin.sh` in the macos-port worktree; the layout
  matters — `Contents/Resources/gstreamer-1.0`, not `Frameworks/`, which makes
  the app unsignable.
- If a feed is fed genuinely undecodable input (two SRT senders bound to one
  port, which happened here via a stray test source), the muxer can leave a
  file whose header never completed. It is reported, not silently dropped, but
  it is not repaired either.

---

# Session 3 notes

## The native app

`wails build -tags desktop` produces `build/bin/liverecord.app`. Verified
running: window opens, feeds record, native folder picker and Reveal wired up,
settings in `~/Library/Application Support/Live Record/config.json`.

Two entry points from one codebase:

| build | entry | engine |
|---|---|---|
| `go build` | `main_cli.go` | gst-launch subprocesses, crash-isolated |
| `wails build -tags desktop` | `main_wails.go` | same today; in-process once wired |

**The MJPEG preview cannot go through the Wails asset server.** WKWebView only
parses `multipart/x-mixed-replace` when CFNetwork splits the parts, and
CFNetwork is not in the path for a custom scheme handler — frames are accepted
and nothing renders, with no error, because the stream never ends. So the app
runs a loopback listener and the page's `<img>` points at that, while the page
and `/api/*` stay on the asset server (Wails' origin validator only accepts IPC
from the start URL's origin, so serving the page over loopback would silently
kill every bound method, including the folder picker).

Packaging bug found and fixed on the way: a `.app` in a user folder *is*
writable, so the writability test happily wrote `config.json` inside
`Contents/MacOS` — invisible, wiped by the next build, and enough to invalidate
the signature. Bundle detection is now by path, not writability.

## Phase status for the composited preview

- **Phase 1 — Wails shell: DONE, verified.**
- **Phase 2 — in-process GStreamer: engine built and proven, not yet wired into
  `Feed`.** `gst_darwin.go` is ~120 lines of cgo: `gst_parse_launch` on the same
  description string `buildPipeline` already produces, `gst_bin_get_by_name`,
  `g_object_set` for the output path and passphrase (the parser's escaping
  cannot be trusted with either), typed bus reads, and
  `gst_video_overlay_set_window_handle`. Tested end to end: records 4.2s of real
  ProRes, reads 21 level messages as typed values rather than scraped regex, and
  finalises via EOS into a valid QuickTime file.
  - The one thing that must not regress: gst-launch's `-e` is gone, so
    `Finalise()` sends EOS and **blocks on the bus for `GST_MESSAGE_EOS` before
    going to NULL**. That ordering is what writes the final index.
  - Remaining wiring: `Feed.runOnce` needs an engine branch. The browser preview
    keeps working unchanged — `fdsink fd=N` can be handed the write end of an
    `os.Pipe` we create in-process, so no `appsink` is needed.
- **Phase 3 — NSView overlay: not started.** The reference is
  an existing in-house GStreamer/Wails overlay implementation. Port notes:
  - Find the host window by **structure, not title**: the visible parentless
    `NSWindow` of this process whose `contentView` has a `WKWebView` subclass in
    its subviews. The reference matches on exact window title and says so
    itself — any `WindowSetTitle` breaks it silently.
  - Add a plain `NSView` as a **sibling of the WKWebView** in the contentView
    (`addSubview:positioned:NSWindowAbove`), and give *that* to `glimagesink`;
    the sink adds its own `GstGLNSView` inside whatever handle it is given.
  - Create it **lazily on first use** — Wails makes its window inside
    `wails.Run`, after `OnStartup` and before `OnDomReady`.
  - Every AppKit call on the main queue. `dispatch_async` everywhere except
    construction, which needs a value back; that one `dispatch_sync` needs two
    guards — `[NSThread isMainThread]`, and `NSApp != nil && [NSApp isRunning]`,
    because being on the main thread is not the same as anyone draining its
    queue (under `go test` nobody is, and it hangs until the timeout).
  - `OnShutdown` runs *after* `-[NSApp run]` returns, so teardown must be
    fire-and-forget: a `dispatch_async` then is never executed.
  - Do not call `gst_element_set_state` from a bound method — it has no timeout
    and runs on the calling goroutine, which would freeze the window.

---

# Session 2 notes

## Session 2 additions

**Settings are editable in the UI now.** Gear → Settings: output folder (with a
Check button that reports writable + free space), file name pattern with live
preview, ProRes flavour, reserve hours, preview mode, and per-feed name + a
pasteable **SRT URL**. Saving stops all feeds and rebuilds them; the panel warns
first. `POST /api/config`, `POST /api/check-folder`.

**Time of day is on screen** — a master clock in the header, driven off the
*server* clock so it matches what the timecode stamper sees, plus per-feed
elapsed and "started at". In-file TOD timecode is still blocked (below).

**Four more silent-failure bugs found and fixed.** Every one of these produced a
plausible-looking recording:

1. `runOnce` never re-checked intent after the probe. Press Stop while a feed is
   probing and there is no process to signal, so it went on to start a recording
   that was already cancelled — a feed that ignores Stop All. Now re-checked
   after the probe and again under the same lock that publishes the process.
2. Context cancellation could not reach a running pipeline: `supervise` only
   tested ctx between iterations, so shutdown blocked forever on a live
   recording. A watcher goroutine now signals the child on cancel.
3. The probe was bounded by **packet count, not time**. Two feeds differing only
   in how compressible their picture was started **7 seconds apart**; on a real
   multi-camera record the ISOs would not start together. It now polls and stops
   the moment the program map is readable — measured 1s apart.
4. **A multi-program transport stream silently recorded the wrong program's
   audio.** Several programs expose audio pads with identical caps, so the
   caps-routed link attached to an arbitrary one — the file had 0.05s of audio
   against 22s of video. `tsdemux` is now pinned to a program number the probe
   chooses (preferring one with both video and audio), and the parser is tested
   for deterministic selection.

Also: `checkFolder` fails a settings save *before* stopping anything if the
folder is not writable, and feeds hold a config snapshot so a reconfigure can
never swap paths under a running recording.

Verified after all of it: both feeds start within 1s, audio tracks video
(20.1s/20.9s and 20.2s/21.4s), meters live, native and browser previews both
working, ProRes HQ `apch`.

---

# Session 1 notes

Everything is stopped. Build is green, tests pass. Nothing is running.

```sh
cd "/Users/sam/Documents/Live Record"
./testsrc.sh &        # two dummy SRT encoders on 9001 / 9002
./liverecord          # UI on http://127.0.0.1:7777
./liverecord -v       # ...and log the full GStreamer pipeline per feed
```

Premiere Pro is still open — I launched it and left it for you; quit it if you
don't want it.

---

## Working and verified

- **SRT caller ingest → growing ProRes.** Two feeds at once, ProRes HQ (`apch`,
  10-bit 4:2:2) + PCM 24-bit, in a flat QuickTime that is valid at every instant
  (`qtmux` robust recording mode). Survives `kill -9`.
- **Reconnect.** A dropped feed resumes into `_pt2`, `_pt3`… and the other feed
  is untouched — each runs in its own process.
- **Stream probe.** The PMT is read from the transport stream to build the
  pipeline deterministically (`probe.go`), replacing `decodebin` auto-plugging.
  Unit-tested against a real capture in `testdata/sample.ts`.
- **Native preview** (`previewMode: "native"`, the default): `glimagesink`
  straight off the decoder — full resolution, full frame rate, no transcode.
- **Browser preview** (`previewMode: "browser"`): scaled MJPEG in the web UI.
  Costs a scale + JPEG encode per feed; useful from another machine.

### Three bugs fixed today, all of which failed silently

1. gst-launch prints errors to **stdout**, not stderr — failures were showing as
   a healthy "recording" while nothing was written.
2. The preview's TCP sink could fail to bind and stop the whole pipeline
   prerolling, killing the recording. It now writes to a pipe on fd 3.
3. **Audio was being lost.** Recordings had 0.1s of audio against 36s of video.
   Cause was in `testsrc.sh`, not the app: no `queue` before the muxer on each
   branch, so the audio thread was throttled by video. Always queue before a mux.

---

## Open — pick up here

### 1. Time-of-day timecode — SOLVED (session 2)

Root-caused and fixed. It was never a timecode bug: the value was always written
correctly into the mdat. `qtmux` rebases every track's chunk offsets exactly
once, guarded by `if (offset == moov->chunks_offset) return;`. The timecode
track is created lazily on the first video buffer carrying a timecode meta, and
`atom_moov_add_trak()` does not inherit the current base — so when audio starts
*ahead of* video in running time (which is what SRT does: audio decodes at once
while video waits for an IDR), the one rebase happens before the timecode track
exists and its offset stays relative forever. It then points into the reserved
header, which is a file hole reading as zeros — hence exactly `00:00:00:00`.

Confirmed on disk here: audio and video chunk offsets landed inside `mdat`
(60,482,522 / 60,776,556) and the timecode track's did not (294,030). Adding the
mdat payload start revealed frames `1,681,011` = 18:40:40, the real record time.

`timecode.go` repairs that one field — during recording (the muxer reintroduces
it on every index update) and again at finalise. Idempotent, refuses malformed
files, and leaves already-correct files alone. Five unit tests.

**Upgrading GStreamer would not have helped**: no timecode or robust-recording
change between 1.26.10 and 1.28.6, and it is still broken on `main`. This is
unreported upstream — the one-line fix is to make `atom_moov_add_trak()` inherit
`moov->chunks_offset`. Worth filing if you want it fixed at source.

Verified end to end: both feeds report an identical `18:52:26:12`, correct while
growing and after finalisation.

### 1b. The original analysis, kept for reference

You asked for TOD timecode in the file, plus elapsed *and* TOD on screen.

`timecodestamper source=rtc` + `qtmux force-create-timecode-trak=true` writes a
correct TOD `tmcd` track. Measured combinations:

| pipeline | TOD in file |
|---|---|
| plain qtmux, video only | ✅ correct |
| plain qtmux, video + audio | ✅ correct |
| **robust qtmux, video only** | ✅ correct |
| **robust qtmux, video + audio** | ❌ `00:00:00:00` |

So robust recording mode (which is what makes the file growable) and an audio
pad do not combine in GStreamer 1.26.10. Tried and did not help:
`reserved-prefill=true`, `start-time-selection=first`, delaying audio with
`min-threshold-time`.

**Re-confirmed in session 2** after the multi-program and audio bugs were fixed
— those were plausible confounders for this result, and they were not the cause.
With audio now healthy (20s of audio against 20s of video) the timecode track is
still `00:00:00:00`.

Options, roughly in order of preference:

- **Try GStreamer 1.28.6** (installed is 1.26.10, `brew` has 1.28.6). Cheapest
  test, may simply be fixed upstream. Do this first.
- **Patch the `tmcd` sample from Go.** It is a single 32-bit big-endian frame
  count in `mdat`; we already parse MOV atoms in `movinfo.go`, so locating and
  rewriting 4 bytes shortly after recording starts is tractable.
- **Accept it and carry TOD elsewhere** — the file's creation time is already
  correct, and the UI can show TOD without the track.

The UI does not yet show TOD at all — it still shows elapsed only. That part is
easy and independent of the above.

### 2. Settings UI — half done

`config.go` is rewritten and working: feeds now take a pasteable **SRT URL**
(`srt://host:port?latency=&streamid=&passphrase=`), plus `filePattern`
(`{name} {date} {time} {datetime}`), `outputDir`, `proresVariant`,
`previewMode`. All validated, all persisted to `config.json`.

Not done: the **HTTP endpoint to save it** (`POST /api/config`, with feeds
stopped and rebuilt on save) and the **settings panel in the UI**. Right now
`config.json` still has to be edited by hand, which is the thing you asked to
fix.

### 3. Wails packaging — not started, and one finding changes the plan

**The MJPEG browser preview cannot go through Wails' asset server.** Verified
against the Wails v2.13.0 module on this machine: its response writers
(`pkg/assetserver/content_type_sniffer.go`,
`pkg/assetserver/webview/responsewriter_darwin.go`) implement no
`http.Flusher`, and WebKit's `SubresourceLoader` sets `m_loadingMultipartContent`
for any `multipart/*` response and then waits for a second `didReceiveResponse`
per part, which `WKURLSchemeTask` never delivers. So "reuse the whole
`http.Handler` unchanged" works for the JSON API but **not** for `/preview/`.

That points the same way you already did: inside the app window the preview
should be the native composited surface, and the MJPEG endpoint stays on the
standalone HTTP server for watching from another machine.



`wails` CLI is at `~/go/bin/wails`, v2.13.0, and `wails doctor` says the machine
is ready. The intended shape, following an existing in-house implementation:

- `main_wails.go` behind `//go:build dev || production || bindings`, current
  `main.go` behind the negation — same split they use.
- `AssetServer: &assetserver.Options{Handler: app.routes()}` reuses the entire
  existing HTTP layer unchanged, MJPEG included, so no frontend rewrite.
- Native folder picker for the output directory via `runtime.OpenDirectoryDialog`.

### 4. Composited preview — decision needed before starting

You want the preview composited into the window like the commentary app, not a
separate window. The macOS implementation is in that project's `macos-port`
an existing in-house GStreamer/Wails overlay implementation: an
NSView added as a sibling of the WKWebView in the Wails contentView, handed to
`glimagesink` via `gst_video_overlay_set_window_handle`, with every AppKit call
marshalled to the main queue.

**The catch:** that call needs in-process GStreamer, so this means moving from
`gst-launch` subprocesses to cgo. That trades away the crash isolation that
currently keeps one feed's failure from touching the other's recording. For a
recorder that is a real cost, though `qtmux` robust mode softens it — a crash
still leaves a valid file missing at most a second.

Middle path worth considering: keep **recording** in isolated subprocesses and
run only the **preview** in-process for compositing. Costs a second decode per
feed (cheap on `vtdec_hw`) but needs the sender to accept two callers.

Today's native `glimagesink` window already gives full resolution, full frame
rate and zero transcode — it just isn't inside the app window.

### 5. Still unverified: Premiere itself

Not once confirmed end to end. Terminal has no Screen Recording permission so I
could not see Premiere's window, and driving it blind risked touching your
projects. Adobe formally documents growing-file support for MXF, not QuickTime —
the files are structurally correct for it and Apple's AVFoundation reads them
mid-write, but **this needs your eyes before a show.** If it refuses, `mxfmux`
is in the same GStreamer install and also carries ProRes.

---

## Notes to self

- Never `go build ... | head -20 && echo OK` — the pipe masks the failure and I
  spent a test cycle on a stale binary. Use `set -e` and no pipe.
- `lsof` truncates the command name to `gst-launc`.
- `textoverlay` with an unlinked text sink pad stalls a live pipeline and looks
  exactly like a dead SRT feed. `timeoverlay text=` is safe.
- macOS `pgrep` has no `-a`.
