package web

import "testing"

// TestWithNoBrowser checks that construction-time options are actually applied
// to the Server. NewServer builds the struct first and applies opts after, so
// an option added to the struct literal by mistake - or a dropped opts loop -
// would leave the flag at its zero value and silently reinstate the browser
// launch this exists to prevent.
func TestWithNoBrowser(t *testing.T) {
	if s := newTestServer(t, testConfig()); s.noBrowser {
		t.Error("noBrowser should default to false when the option isn't passed")
	}

	s := newTestServer(t, testConfig(), WithNoBrowser())
	if !s.noBrowser {
		t.Error("WithNoBrowser() did not take effect")
	}
}
