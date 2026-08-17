//go:build darwin && cgo

// gst_darwin.go runs GStreamer inside this process instead of shelling out to
// gst-launch-1.0.
//
// # Why
//
// gst_video_overlay_set_window_handle() has to be called on the sink element,
// which means the element must live in our address space. That is the only
// reason this file exists — compositing the picture into the app window is not
// possible with a child process.
//
// # What it costs, and what replaces it
//
// The subprocess engine got two things from gst-launch that must now be done by
// hand, and the first one is the dangerous one:
//
//	-e  On SIGINT, gst-launch pushes EOS and waits for it to reach the sink
//	    before going to NULL. That is what finalises the moov. Here we send the
//	    EOS event ourselves and *block on the bus for GST_MESSAGE_EOS* before
//	    changing state. Get that ordering wrong and every recording ends
//	    unfinalised.
//	-m  Bus messages were printed and scraped with regular expressions. Here we
//	    read the bus directly and pull typed fields out of the message
//	    structure, which is both cheaper and less fragile.
//
// # Threading
//
// Nothing here touches AppKit. Bus polling runs on its own goroutine.
// gst_element_set_state() runs every element's state change on the calling
// goroutine and takes no timeout, so it must never be called from the Wails
// message loop — the window would stop repainting for the duration.
package main

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-video-1.0
#include <gst/gst.h>
#include <gst/video/videooverlay.h>
#include <stdlib.h>

// Set a string property on an element by name. Paths and passphrases go through
// here rather than the pipeline description, because the parser has its own
// escaping rules and a path is at the mercy of them.
static void lr_set_string(GstElement *bin, const char *elem, const char *prop, const char *val) {
    GstElement *e = gst_bin_get_by_name(GST_BIN(bin), elem);
    if (!e) return;
    g_object_set(G_OBJECT(e), prop, val, NULL);
    gst_object_unref(e);
}

static GstElement *lr_get_by_name(GstElement *bin, const char *name) {
    return gst_bin_get_by_name(GST_BIN(bin), name);
}

static void lr_overlay_set_handle(GstElement *sink, guintptr handle) {
    if (!sink) return;
    if (!GST_IS_VIDEO_OVERLAY(sink)) return;
    gst_video_overlay_set_window_handle(GST_VIDEO_OVERLAY(sink), handle);
}

// Pull one message. Returns NULL on timeout.
static GstMessage *lr_bus_pop(GstElement *pipeline, GstClockTime timeout) {
    GstBus *bus = gst_pipeline_get_bus(GST_PIPELINE(pipeline));
    if (!bus) return NULL;
    GstMessage *msg = gst_bus_timed_pop(bus, timeout);
    gst_object_unref(bus);
    return msg;
}

static const char *lr_msg_src_name(GstMessage *m) {
    return (m && GST_MESSAGE_SRC(m)) ? GST_OBJECT_NAME(GST_MESSAGE_SRC(m)) : "";
}

// Error text, caller frees.
static char *lr_msg_error(GstMessage *m) {
    GError *err = NULL; gchar *dbg = NULL; char *out = NULL;
    if (GST_MESSAGE_TYPE(m) == GST_MESSAGE_ERROR)        gst_message_parse_error(m, &err, &dbg);
    else if (GST_MESSAGE_TYPE(m) == GST_MESSAGE_WARNING) gst_message_parse_warning(m, &err, &dbg);
    else return NULL;
    out = g_strdup_printf("%s%s%s", err ? err->message : "unknown",
                          dbg ? " | " : "", dbg ? dbg : "");
    if (err) g_error_free(err);
    if (dbg) g_free(dbg);
    return out;
}

// The level element's RMS array, first two channels. Returns how many it wrote.
//
// Two array types have to be handled. GStreamer's own GST_TYPE_ARRAY is the
// obvious one, but level actually posts a plain GLib GValueArray — which is
// what the serialised message shows as "rms=(GValueArray)< ... >". Checking
// only for GST_TYPE_ARRAY silently yields no values and leaves every meter
// pinned at the floor.
static int lr_msg_level_rms(GstMessage *m, double *out, int max) {
    const GstStructure *s = gst_message_get_structure(m);
    if (!s || !gst_structure_has_name(s, "level")) return 0;
    const GValue *arr = gst_structure_get_value(s, "rms");
    if (!arr) return 0;
    int written = 0;

    if (GST_VALUE_HOLDS_ARRAY(arr)) {
        guint n = gst_value_array_get_size(arr);
        for (guint i = 0; i < n && written < max; i++) {
            const GValue *v = gst_value_array_get_value(arr, i);
            if (v && G_VALUE_HOLDS_DOUBLE(v)) out[written++] = g_value_get_double(v);
        }
        return written;
    }

G_GNUC_BEGIN_IGNORE_DEPRECATIONS
    if (G_VALUE_HOLDS(arr, G_TYPE_VALUE_ARRAY)) {
        GValueArray *va = (GValueArray *)g_value_get_boxed(arr);
        if (va) {
            for (guint i = 0; i < va->n_values && written < max; i++) {
                GValue *v = g_value_array_get_nth(va, i);
                if (v && G_VALUE_HOLDS_DOUBLE(v)) out[written++] = g_value_get_double(v);
            }
        }
    }
G_GNUC_END_IGNORE_DEPRECATIONS
    return written;
}

static gboolean lr_msg_is_level(GstMessage *m) {
    const GstStructure *s = gst_message_get_structure(m);
    return s && gst_structure_has_name(s, "level");
}

static gboolean lr_msg_is_progress_element(GstMessage *m) {
    const GstStructure *s = gst_message_get_structure(m);
    return s && gst_structure_has_name(s, "progress");
}

// A live pipeline has to be brought up through PAUSED and have its latency
// recalculated when elements ask, exactly as gst-launch does. Going straight to
// PLAYING and ignoring GST_MESSAGE_LATENCY leaves the latency unconfigured, and
// the sinks then treat everything as late.
static int lr_get_state(GstElement *p, GstClockTime timeout) {
    GstState st, pending;
    return (int)gst_element_get_state(p, &st, &pending, timeout);
}
static int lr_current_state(GstElement *p) {
    GstState st, pending;
    gst_element_get_state(p, &st, &pending, 0);
    return (int)st;
}
static int lr_recalc_latency(GstElement *p) {
    return gst_bin_recalculate_latency(GST_BIN(p)) ? 1 : 0;
}

// GST_MESSAGE_TYPE is a macro, so cgo cannot reach it directly.
static int lr_msg_type(GstMessage *m)  { return (int)GST_MESSAGE_TYPE(m); }
static int lr_type_eos(void)      { return (int)GST_MESSAGE_EOS; }
static int lr_type_error(void)    { return (int)GST_MESSAGE_ERROR; }
static int lr_type_warning(void)  { return (int)GST_MESSAGE_WARNING; }
static int lr_type_element(void)  { return (int)GST_MESSAGE_ELEMENT; }
static int lr_type_progress(void) { return (int)GST_MESSAGE_PROGRESS; }
static int lr_type_latency(void)  { return (int)GST_MESSAGE_LATENCY; }
static int lr_type_clocklost(void){ return (int)GST_MESSAGE_CLOCK_LOST; }
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"
)

var gstInitOnce sync.Once

// gstInit initialises GStreamer exactly once for the process.
func gstInit() {
	gstInitOnce.Do(func() {
		C.gst_init(nil, nil)
	})
}

// gstMsgKind is the subset of bus traffic this app acts on.
type gstMsgKind int

const (
	gstMsgOther gstMsgKind = iota
	gstMsgEOS
	gstMsgError
	gstMsgWarning
	gstMsgLevel
	gstMsgProgress
	gstMsgLatency
	gstMsgClockLost
)

type gstMessage struct {
	Kind gstMsgKind
	Text string
	RMS  [2]float64
	Src  string
}

// gstPipeline is one running pipeline.
type gstPipeline struct {
	mu       sync.Mutex
	pipeline *C.GstElement
	closed   bool
}

// gstParse builds a pipeline from the same description string the subprocess
// engine passes to gst-launch.
func gstParse(desc string) (*gstPipeline, error) {
	gstInit()

	cdesc := C.CString(desc)
	defer C.free(unsafe.Pointer(cdesc))

	var gerr *C.GError
	el := C.gst_parse_launch(cdesc, &gerr)
	if gerr != nil {
		msg := C.GoString((*C.char)(gerr.message))
		C.g_error_free(gerr)
		if el != nil {
			C.gst_object_unref(C.gpointer(unsafe.Pointer(el)))
		}
		// gst_parse_launch reports one error with an offset, unlike gst-launch
		// which names the failing element, so log the description too.
		return nil, fmt.Errorf("%s (pipeline: %s)", msg, desc)
	}
	if el == nil {
		return nil, errors.New("gst_parse_launch returned no pipeline")
	}
	return &gstPipeline{pipeline: el}, nil
}

// SetString sets a string property on a named element after parsing. Used for
// the output path and the SRT passphrase, which must not go through the
// description parser's escaping.
func (p *gstPipeline) SetString(element, prop, value string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pipeline == nil {
		return
	}
	ce, cp, cv := C.CString(element), C.CString(prop), C.CString(value)
	defer C.free(unsafe.Pointer(ce))
	defer C.free(unsafe.Pointer(cp))
	defer C.free(unsafe.Pointer(cv))
	C.lr_set_string(p.pipeline, ce, cp, cv)
}

// SetWindowHandle hands a native view to the named video sink. This is the
// whole reason the pipeline runs in-process.
func (p *gstPipeline) SetWindowHandle(element string, handle uintptr) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pipeline == nil {
		return errors.New("pipeline is gone")
	}
	ce := C.CString(element)
	defer C.free(unsafe.Pointer(ce))
	sink := C.lr_get_by_name(p.pipeline, ce)
	if sink == nil {
		return fmt.Errorf("no element named %q in the pipeline", element)
	}
	defer C.gst_object_unref(C.gpointer(unsafe.Pointer(sink)))
	C.lr_overlay_set_handle(sink, C.guintptr(handle))
	return nil
}

func (p *gstPipeline) setState(st C.GstState) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pipeline == nil {
		return errors.New("pipeline is gone")
	}
	if C.gst_element_set_state(p.pipeline, st) == C.GST_STATE_CHANGE_FAILURE {
		return fmt.Errorf("state change to %d failed", int(st))
	}
	return nil
}

// Play brings the pipeline up the way a live application must: through PAUSED,
// waiting for that transition to settle, then to PLAYING.
//
// Going straight to PLAYING is the obvious shortcut and it is wrong for a live
// pipeline with several sinks: latency is never distributed, the muxer's
// latency query fails, and the sinks judge every buffer late. Symptom is a
// pipeline that reports PLAYING and writes nothing.
func (p *gstPipeline) Play() error {
	if err := p.setState(C.GST_STATE_PAUSED); err != nil {
		return err
	}
	p.mu.Lock()
	pl := p.pipeline
	p.mu.Unlock()
	if pl != nil {
		// A live source returns NO_PREROLL rather than SUCCESS; either is fine,
		// what matters is that the transition has completed.
		C.lr_get_state(pl, C.GstClockTime(5*time.Second/time.Nanosecond))
	}
	return p.setState(C.GST_STATE_PLAYING)
}

// RecalculateLatency re-runs latency distribution. Elements post
// GST_MESSAGE_LATENCY when their own latency changes — a decoder settling, an
// encoder starting — and the application is required to act on it.
func (p *gstPipeline) RecalculateLatency() bool {
	p.mu.Lock()
	pl := p.pipeline
	p.mu.Unlock()
	if pl == nil {
		return false
	}
	return C.lr_recalc_latency(pl) != 0
}

// StateName reports the pipeline's current state, for logging.
func (p *gstPipeline) StateName() string {
	p.mu.Lock()
	pl := p.pipeline
	p.mu.Unlock()
	if pl == nil {
		return "gone"
	}
	switch int(C.lr_current_state(pl)) {
	case 1:
		return "NULL"
	case 2:
		return "READY"
	case 3:
		return "PAUSED"
	case 4:
		return "PLAYING"
	}
	return "?"
}
func (p *gstPipeline) Pause() error { return p.setState(C.GST_STATE_PAUSED) }
func (p *gstPipeline) Null() error  { return p.setState(C.GST_STATE_NULL) }

// Poll waits up to timeout for one bus message.
func (p *gstPipeline) Poll(timeout time.Duration) (gstMessage, bool) {
	p.mu.Lock()
	pl := p.pipeline
	p.mu.Unlock()
	if pl == nil {
		return gstMessage{}, false
	}

	msg := C.lr_bus_pop(pl, C.GstClockTime(timeout.Nanoseconds()))
	if msg == nil {
		return gstMessage{}, false
	}
	defer C.gst_message_unref(msg)

	out := gstMessage{Src: C.GoString(C.lr_msg_src_name(msg))}
	switch t := int(C.lr_msg_type(msg)); t {
	case int(C.lr_type_eos()):
		out.Kind = gstMsgEOS
	case int(C.lr_type_error()), int(C.lr_type_warning()):
		out.Kind = gstMsgError
		if t == int(C.lr_type_warning()) {
			out.Kind = gstMsgWarning
		}
		if c := C.lr_msg_error(msg); c != nil {
			out.Text = C.GoString(c)
			C.g_free(C.gpointer(unsafe.Pointer(c)))
		}
	case int(C.lr_type_element()):
		switch {
		case C.lr_msg_is_level(msg) != 0:
			out.Kind = gstMsgLevel
			var vals [2]C.double
			n := int(C.lr_msg_level_rms(msg, &vals[0], 2))
			out.RMS = [2]float64{-96, -96}
			for i := 0; i < n; i++ {
				out.RMS[i] = float64(vals[i])
			}
			if n == 1 {
				out.RMS[1] = out.RMS[0] // mono: mirror to both meters
			}
		case C.lr_msg_is_progress_element(msg) != 0:
			out.Kind = gstMsgProgress
		}
	case int(C.lr_type_progress()):
		out.Kind = gstMsgProgress
	case int(C.lr_type_latency()):
		out.Kind = gstMsgLatency
	case int(C.lr_type_clocklost()):
		out.Kind = gstMsgClockLost
	}
	return out, true
}

// SendEOS pushes end-of-stream into the pipeline. The caller must then wait for
// GST_MESSAGE_EOS to come back up the bus before going to NULL — that round
// trip is what makes the muxer write its final index.
func (p *gstPipeline) SendEOS() {
	p.mu.Lock()
	pl := p.pipeline
	p.mu.Unlock()
	if pl != nil {
		C.gst_element_send_event(pl, C.gst_event_new_eos())
	}
}

// Finalise performs the shutdown that gst-launch's -e flag performed: push EOS,
// wait for it to come back up the bus, and only then go to NULL.
//
// Skipping the wait truncates the file's final index. Robust recording mode
// makes that survivable rather than fatal, but the last seconds would be lost.
func (p *gstPipeline) Finalise(timeout time.Duration) error {
	p.mu.Lock()
	pl := p.pipeline
	p.mu.Unlock()
	if pl == nil {
		return nil
	}

	C.gst_element_send_event(pl, C.gst_event_new_eos())

	deadline := time.Now().Add(timeout)
	for time.Since(deadline) < 0 {
		msg, ok := p.Poll(200 * time.Millisecond)
		if !ok {
			continue
		}
		if msg.Kind == gstMsgEOS {
			return p.Null()
		}
		if msg.Kind == gstMsgError {
			// An error after EOS still means no more data is coming.
			break
		}
	}
	// Timed out: go to NULL anyway. The file stays valid thanks to the
	// periodically rewritten index, but say so rather than pretending.
	err := p.Null()
	if err == nil {
		err = fmt.Errorf("EOS did not arrive within %s; the last index update is the end of the file", timeout)
	}
	return err
}

// Close releases the pipeline. Safe to call more than once.
func (p *gstPipeline) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pipeline == nil || p.closed {
		return
	}
	p.closed = true
	C.gst_element_set_state(p.pipeline, C.GST_STATE_NULL)
	C.gst_object_unref(C.gpointer(unsafe.Pointer(p.pipeline)))
	p.pipeline = nil
}
