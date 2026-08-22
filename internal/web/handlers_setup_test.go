package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSetupRendersWithoutConfig covers the wizard's defining condition: it
// runs before a config exists, so getConfig() returns nil. layout.html does
// {{if gt (len .Profiles) 1}} for the profile switcher, and a missing
// Profiles key made that fail mid-render ("error calling len: reflect: call
// of reflect.Value.Type on zero Value"), leaving the page blank below the
// nav bar. renderWithCSRF must set the switcher keys even with no config.
func TestSetupRendersWithoutConfig(t *testing.T) {
	s := newTestServer(t, nil)

	for _, path := range []string{"/setup", "/setup/profile"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		switch path {
		case "/setup":
			s.handleSetupWelcome(rec, req)
		default:
			s.handleSetupProfile(rec, req)
		}

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", path, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if strings.Contains(body, "Template error") {
			t.Errorf("%s: template failed to render: %s", path, body)
		}
		// The layout closes with </html> only if rendering ran to
		// completion - a mid-render failure still emits the nav bar.
		if !strings.Contains(body, "</html>") {
			t.Errorf("%s: page truncated mid-render, got %d bytes", path, len(body))
		}
	}
}
