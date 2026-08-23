package main

import (
	"fmt"
	"os"

	"github.com/eraser-privacy/eraser/internal/email"
)

// captureDir is bound to the --capture-dir persistent flag in main.go.
var captureDir string

// resolveCaptureDir returns the directory sends should be recorded to, or ""
// to send for real. The flag wins over the environment so a command line can
// always override an exported variable.
func resolveCaptureDir() string {
	if captureDir != "" {
		return captureDir
	}
	return os.Getenv("ERASER_CAPTURE_DIR")
}

// newCaptureSenderIfRequested builds the process-wide capture sender when
// capture mode is on, returning nil when it isn't.
//
// Capture mode is exactly "a capture directory was given" - there is
// deliberately no separate boolean. One knob with one meaning, and its value
// is the thing a caller needs anyway in order to read the results back.
//
// One sender is shared by every send path in the process so that sequence
// numbers and manifest.jsonl form a single ordered record of the whole run,
// including the setup wizard's test send.
func newCaptureSenderIfRequested() (*email.CaptureSender, error) {
	dir := resolveCaptureDir()
	if dir == "" {
		return nil, nil
	}

	cs, err := email.NewCaptureSender(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to enable capture mode: %w", err)
	}

	// Announce loudly. The worst way for this feature to fail is silently, in
	// either direction: a test run that believes it is capturing while
	// actually talking to a real mail server, or a person who thinks their
	// removal requests went out when they were only written to disk.
	fmt.Printf("⚠️  CAPTURE MODE - no email will be sent; messages are written to %s\n", cs.Dir())
	return cs, nil
}
