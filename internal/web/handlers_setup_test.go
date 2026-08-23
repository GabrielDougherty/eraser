package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
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

// TestSetupProfilePostPersistsToNewSession pins the first POST of the
// wizard, where the session is created during that same request. The
// handler used to save the profile with updateSession(r, ...), which keys
// off the request's session cookie - but on this request the cookie exists
// only on the response, so the update silently no-opped, the profile was
// never stored, and /setup/email bounced straight back to /setup/profile:
// the wizard could never advance past step 2.
func TestSetupProfilePostPersistsToNewSession(t *testing.T) {
	s := newTestServer(t, nil)

	form := url.Values{
		"first_name": {"Anna"},
		"last_name":  {"Popena"},
		"email":      {"anna@example.com"},
	}
	req := httptest.NewRequest(http.MethodPost, "/setup/profile", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleSetupProfile(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/setup/email" {
		t.Fatalf("expected redirect to /setup/email, got %q", loc)
	}

	// Replay the response's session cookie the way a browser would.
	res := rec.Result()
	defer func() { _ = res.Body.Close() }()
	var sessionCookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == "eraser_session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("no eraser_session cookie set on the profile POST response")
	}

	session := s.sessions.Get(sessionCookie.Value)
	if session == nil {
		t.Fatal("session cookie points at no stored session")
	}
	if session.Profile.FirstName != "Anna" || session.Profile.Email != "anna@example.com" {
		t.Fatalf("profile not stored in session: %+v", session.Profile)
	}

	// The next step must now render rather than redirect back to step 2.
	emailReq := httptest.NewRequest(http.MethodGet, "/setup/email", nil)
	emailReq.AddCookie(sessionCookie)
	emailRec := httptest.NewRecorder()
	s.handleSetupEmail(emailRec, emailReq)

	if emailRec.Code != http.StatusOK {
		t.Fatalf("/setup/email: expected 200, got %d (Location: %q)",
			emailRec.Code, emailRec.Header().Get("Location"))
	}
}
