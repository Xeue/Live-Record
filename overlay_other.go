//go:build !(darwin && cgo && (dev || production || bindings))

package main

// newPreviewOverlay returns nil where there is no window to composite into —
// the command-line build, and any non-macOS target. Each video sink then opens
// its own window, which is the pre-compositing behaviour.
func newPreviewOverlay() previewOverlay { return nil }
