//go:build darwin && cgo && (dev || production || bindings)

// overlay_darwin.go is the native surface each feed's picture is drawn on: a
// plain NSView added to the Wails window's contentView as a SIBLING of the
// WKWebView, which glimagesink renders into through GstVideoOverlay.
//
// # Why a view of our own
//
// The sink ADDS A SUBVIEW to whatever handle it is given and removes it again
// on teardown. Handed the contentView it would be a sibling of the WKWebView
// with a frame nobody owns; handed the WKWebView it would be a subview of a
// view WebKit reserves entirely to itself. So it gets a view that exists only
// to be moved and hidden, paints nothing but its own black layer, and knows
// nothing about GStreamer.
//
// # Finding the window, by structure and not by title
//
// There is no public Wails v2 API that hands you the NSWindow. The reference
// implementation matches on the exact window title, and says itself that any
// later WindowSetTitle breaks it silently — the picture simply never appears.
// This looks for the visible, parentless window of this process whose
// contentView contains a WKWebView instead, which survives a title change.
//
// # Threading, which is the part that can hang the application
//
// EVERY APPKIT CALL BELOW RUNS ON THE MAIN THREAD. That is AppKit's rule, and
// GStreamer breaks it by default: bus messages arrive on streaming threads and
// Wails' bound methods run on a dispatcher goroutine. So every entry point is
// "record the wish, dispatch to the main queue, return".
//
// dispatch_async everywhere except construction, which has to return a value.
// That one dispatch_sync needs two guards:
//
//   - [NSThread isMainThread], because dispatching synchronously onto the queue
//     you are already running on deadlocks instantly; and
//   - NSApp != nil && [NSApp isRunning], because being ON the main thread is not
//     the same question as whether anyone is DRAINING its queue. Under `go test`
//     nobody is, and the dispatch_sync would hang until the test binary is
//     killed.
//
// Teardown is fire-and-forget: Wails runs OnShutdown after -[NSApp run] has
// already returned, at which point a block posted to the main queue is never
// executed. The cost of that is one unreleased NSView in a process that is
// exiting.
package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa -framework WebKit
#import <Cocoa/Cocoa.h>

// Is there a main loop actually servicing the queue?
static int lr_main_loop_running(void) {
    // Reading NSApp does not instantiate it, unlike [NSApplication sharedApplication].
    return (NSApp != nil && [NSApp isRunning]) ? 1 : 0;
}

// The host window: visible, parentless, and containing a WKWebView. Must run on
// the main thread.
static NSWindow *lr_find_host_on_main(void) {
    Class webViewClass = NSClassFromString(@"WKWebView");
    NSWindow *fallback = nil;
    for (NSWindow *w in [NSApp windows]) {
        if (![w isVisible]) continue;
        if ([w parentWindow] != nil) continue;
        NSView *content = [w contentView];
        if (content == nil) continue;
        if (fallback == nil) fallback = w;
        if (webViewClass == nil) continue;
        for (NSView *sub in [content subviews]) {
            if ([sub isKindOfClass:webViewClass]) return w;
        }
    }
    // A window without a WKWebView subview is still better than nothing: Wails
    // may not have attached it yet.
    return fallback;
}

// Create the overlay view and return it. Main thread only.
static void *lr_overlay_create_on_main(void) {
    NSWindow *host = lr_find_host_on_main();
    if (host == nil) return NULL;
    NSView *content = [host contentView];
    if (content == nil) return NULL;

    NSView *v = [[NSView alloc] initWithFrame:NSMakeRect(0, 0, 16, 9)];
    [v setWantsLayer:YES];
    [[v layer] setBackgroundColor:[[NSColor blackColor] CGColor]];
    [v setHidden:YES];
    // Track the window during a live resize. The page reports a new rectangle
    // on every layout, but that round trip is asynchronous — page to Go to the
    // main queue — so between updates the surface would sit at its old frame
    // and visibly slide out of the layout until something forced a redraw.
    // A fully flexible autoresizing mask keeps it proportionally in place for
    // those in-between frames; the explicit SetRect then corrects it exactly.
    [v setAutoresizingMask:NSViewWidthSizable | NSViewHeightSizable |
                           NSViewMinXMargin  | NSViewMaxXMargin |
                           NSViewMinYMargin  | NSViewMaxYMargin];
    // Above the web view in z-order, so the picture is not painted over.
    [content addSubview:v positioned:NSWindowAbove relativeTo:nil];
    return (void *)v;
}

static void *lr_overlay_create(void) {
    if ([NSThread isMainThread]) {
        return lr_overlay_create_on_main();
    }
    if (!lr_main_loop_running()) {
        return NULL;   // nobody would ever run the block
    }
    __block void *out = NULL;
    dispatch_sync(dispatch_get_main_queue(), ^{
        out = lr_overlay_create_on_main();
    });
    return out;
}

// Position in window coordinates with a TOP-LEFT origin, which is what the page
// reports. AppKit's origin is bottom-left, so flip against the superview.
static void lr_overlay_set_rect(void *view, int x, int y, int w, int h) {
    if (view == NULL) return;
    NSView *v = (NSView *)view;   // retained by the block until it runs
    dispatch_async(dispatch_get_main_queue(), ^{
        NSView *super = [v superview];
        if (super == nil) return;
        CGFloat flipped = [super bounds].size.height - (CGFloat)y - (CGFloat)h;
        [v setFrame:NSMakeRect((CGFloat)x, flipped, (CGFloat)w, (CGFloat)h)];
    });
}

static void lr_overlay_set_visible(void *view, int visible) {
    if (view == NULL) return;
    NSView *v = (NSView *)view;
    dispatch_async(dispatch_get_main_queue(), ^{
        [v setHidden:visible ? NO : YES];
    });
}

static void lr_overlay_close(void *view) {
    if (view == NULL) return;
    NSView *v = (NSView *)view;
    dispatch_async(dispatch_get_main_queue(), ^{
        [v removeFromSuperview];
    });
}
*/
import "C"

import (
	"sync"
	"unsafe"
)

// macOverlay owns one native view per feed.
type macOverlay struct {
	mu    sync.Mutex
	views map[string]unsafe.Pointer
	rects map[string][4]int
}

func newMacOverlay() *macOverlay {
	return &macOverlay{
		views: make(map[string]unsafe.Pointer),
		rects: make(map[string][4]int),
	}
}

// Handle returns the feed's view, creating it on first use.
//
// Creation is lazy on purpose: Wails builds its window inside wails.Run, after
// OnStartup and before OnDomReady, and there is no callback guaranteed to run
// with the window present. Asking early simply returns 0, and the caller falls
// back to a separate window for that run.
func (o *macOverlay) Handle(feed string) uintptr {
	o.mu.Lock()
	defer o.mu.Unlock()
	if v, ok := o.views[feed]; ok && v != nil {
		return uintptr(v)
	}
	v := C.lr_overlay_create()
	if v == nil {
		return 0
	}
	o.views[feed] = unsafe.Pointer(v)
	// Apply any rectangle the page reported before the view existed.
	if r, ok := o.rects[feed]; ok {
		C.lr_overlay_set_rect(v, C.int(r[0]), C.int(r[1]), C.int(r[2]), C.int(r[3]))
		C.lr_overlay_set_visible(v, 1)
	}
	return uintptr(v)
}

func (o *macOverlay) SetRect(feed string, x, y, w, h int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if r, ok := o.rects[feed]; ok && r == [4]int{x, y, w, h} {
		return // coalesce: the page reports on every layout
	}
	o.rects[feed] = [4]int{x, y, w, h}
	if v, ok := o.views[feed]; ok && v != nil {
		C.lr_overlay_set_rect(v, C.int(x), C.int(y), C.int(w), C.int(h))
	}
}

func (o *macOverlay) SetVisible(feed string, visible bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	v, ok := o.views[feed]
	if !ok || v == nil {
		return
	}
	n := C.int(0)
	if visible {
		n = 1
	}
	C.lr_overlay_set_visible(v, n)
}

func (o *macOverlay) Close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for name, v := range o.views {
		if v != nil {
			C.lr_overlay_close(v)
		}
		delete(o.views, name)
	}
}

// newPreviewOverlay is what the app calls; separate so the non-macOS build can
// return nil without this file existing.
func newPreviewOverlay() previewOverlay { return newMacOverlay() }
