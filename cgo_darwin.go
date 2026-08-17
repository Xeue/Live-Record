//go:build darwin && (dev || production || bindings)

// cgo_darwin.go carries link flags only. Scoped to the Wails builds so a plain
// `go build` of the CLI stays pure Go.
//
// Wails v2.13.0 references UTType but does not link UniformTypeIdentifiers, so
// against the macOS 26 SDK the build fails with:
//
//	Undefined symbols for architecture arm64:
//	  "_OBJC_CLASS_$_UTType", referenced from: ...
package main

/*
#cgo LDFLAGS: -framework UniformTypeIdentifiers
*/
import "C"
