//go:build !(darwin && cgo)

package main

import (
	"context"
	"time"
)

// inProcessAvailable is false without cgo: gst_video_overlay_set_window_handle
// needs GStreamer linked into this process, so this build uses the subprocess
// engine only. Nothing calls runInProcess, but it must exist to compile.
const inProcessAvailable = false

func (f *Feed) runInProcess(context.Context, State, StreamInfo, string, string) time.Duration {
	panic("in-process engine is not available in this build")
}
