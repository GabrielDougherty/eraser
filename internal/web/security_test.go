package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eraser-privacy/eraser/internal/config"
)

// TestSecurityHeaders pins the response headers every page carries. The
// end-to-end suite drives the UI through a browser but never inspects
// headers, so nothing else would notice if one were dropped - and each of
// these is doing a specific job for an app that renders
// attacker-influenced content (broker reply bodies) and holds a live mail
// credential.
func TestSecurityHeaders(t *testing.T) {
	handler := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))

	want := map[string]string{
		// The UI has no business being framed, and clickjacking a page whose
		// buttons send 700 emails or wipe history is worth ruling out.
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	}
	for header, expected := range want {
		if got := rec.Header().Get(header); got != expected {
			t.Errorf("%s = %q, want %q", header, got, expected)
		}
	}

	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no Content-Security-Policy set")
	}
	for _, directive := range []string{
		"default-src 'self'",
		"object-src 'none'",
		"frame-ancestors 'none'",
		"form-action 'self'", // stops a form on this page POSTing off-site
		"base-uri 'self'",
	} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP is missing %q: %s", directive, csp)
		}
	}
}

// TestSecurityHeadersCacheControl covers the split: pages may hold a profile
// and mail settings and must not be written to a shared cache, while static
// assets are large, immutable and cached deliberately.
func TestSecurityHeadersCacheControl(t *testing.T) {
	handler := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if cc := page.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("a page carrying settings was not marked no-store: %q", cc)
	}

	asset := httptest.NewRecorder()
	handler.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/static/js/htmx.min.js", nil))
	if cc := asset.Header().Get("Cache-Control"); strings.Contains(cc, "no-store") {
		t.Errorf("a static asset was marked no-store: %q", cc)
	}
}

// TestSwitchProfileRejectsOffsiteRedirects covers the open-redirect guard.
// The redirect target is a form field, so a link that submits this form
// could otherwise bounce the user to an attacker's page from a URL that
// legitimately starts on their own machine - the classic setup for a
// convincing credential-phishing page.
func TestSwitchProfileRejectsOffsiteRedirects(t *testing.T) {
	s := newTestServer(t, testConfig("me", "spouse"))

	hostile := []string{
		"//evil.example",          // protocol-relative: browsers treat this as off-site
		"//evil.example/settings", //
		"https://evil.example",    // absolute
		"http://evil.example",     //
		"evil.example",            // no leading slash
		"",                        // absent
	}

	for _, target := range hostile {
		t.Run(target, func(t *testing.T) {
			form := url.Values{"profile_id": {"me"}, "redirect": {target}}
			req := httptest.NewRequest(http.MethodPost, "/api/profile", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()

			s.handleAPISwitchProfile(rec, req)

			if got := rec.Header().Get("Location"); got != "/" {
				t.Errorf("redirect %q was not rewritten to %q, got %q", target, "/", got)
			}
		})
	}
}

func TestSwitchProfileKeepsSameSiteRedirects(t *testing.T) {
	s := newTestServer(t, testConfig("me", "spouse"))

	form := url.Values{"profile_id": {"spouse"}, "redirect": {"/brokers?status=failed"}}
	req := httptest.NewRequest(http.MethodPost, "/api/profile", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	s.handleAPISwitchProfile(rec, req)

	if got := rec.Header().Get("Location"); got != "/brokers?status=failed" {
		t.Errorf("a same-site redirect was rewritten: %q", got)
	}

	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == activeProfileCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("switching profile set no cookie")
	}
	if cookie.Value != "spouse" {
		t.Errorf("cookie value = %q, want spouse", cookie.Value)
	}
}

// TestSwitchProfileIgnoresUnknownProfiles matters because the cookie decides
// whose identity subsequent sends go out under. Accepting an arbitrary value
// would leave activeProfile resolving against a profile that doesn't exist.
func TestSwitchProfileIgnoresUnknownProfiles(t *testing.T) {
	s := newTestServer(t, testConfig("me", "spouse"))

	form := url.Values{"profile_id": {"attacker"}, "redirect": {"/"}}
	req := httptest.NewRequest(http.MethodPost, "/api/profile", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	s.handleAPISwitchProfile(rec, req)

	for _, c := range rec.Result().Cookies() {
		if c.Name == activeProfileCookie {
			t.Errorf("an unknown profile id was written to the cookie: %q", c.Value)
		}
	}
}

// TestActiveProfileFallsBackWhenCookieIsStale covers the read side: a cookie
// naming a profile that has since been deleted from the config must resolve
// to a real profile rather than an empty one, or sends would go out with no
// name and no address.
func TestActiveProfileFallsBackWhenCookieIsStale(t *testing.T) {
	s := newTestServer(t, testConfig("me", "spouse"))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: activeProfileCookie, Value: "deleted-profile"})

	got := s.activeProfile(req)
	if got.ID != "me" {
		t.Errorf("stale cookie resolved to %q, want the first configured profile", got.ID)
	}
	if got.Email == "" {
		t.Error("resolved to a profile with no email; sends would have no sender identity")
	}
}

func TestActiveProfileHonoursTheCookie(t *testing.T) {
	s := newTestServer(t, testConfig("me", "spouse"))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: activeProfileCookie, Value: "spouse"})

	if got := s.activeProfile(req); got.ID != "spouse" {
		t.Errorf("activeProfile = %q, want spouse", got.ID)
	}
}

func TestActiveProfileWithNoConfig(t *testing.T) {
	// The setup wizard renders before any config exists; resolving must not
	// panic and must yield the default id the layout expects.
	s := newTestServer(t, nil)
	if got := s.activeProfile(httptest.NewRequest(http.MethodGet, "/setup", nil)); got.ID != config.DefaultProfileID {
		t.Errorf("activeProfile with no config = %q, want %q", got.ID, config.DefaultProfileID)
	}
}
