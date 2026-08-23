package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eraser-privacy/eraser/internal/config"
	"github.com/go-chi/chi/v5"
)

// withURLParam attaches a chi URL param to a request the way the router
// would after matching a route like "/settings/profiles/{profileID}/edit" -
// needed here since these tests call the handler directly rather than
// through the full router.
func withURLParam(r *http.Request, key, value string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, value)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// config.SlugifyProfileID's own behavior (basic slugify, collisions,
// diacritics) is covered by internal/config's tests now that the web
// package no longer has its own copy of that logic - see
// internal/config/config_test.go.

func TestHandleSettingsProfileNewCreatesProfile(t *testing.T) {
	s := newTestServer(t, testConfig())
	s.configPath = filepath.Join(t.TempDir(), "config.yaml")

	// GET should render without error (regression check: this panicked
	// with "index of untyped nil" before the handler set an Errors key
	// on the no-error GET path, since the template does {{index .Errors "_"}}).
	getReq := httptest.NewRequest(http.MethodGet, "/settings/profiles/new", nil)
	getRec := httptest.NewRecorder()
	s.handleSettingsProfileNew(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", getRec.Code, getRec.Body.String())
	}

	form := url.Values{
		"first_name":  {"Anna"},
		"middle_name": {"Marija"},
		"last_name":   {"Popena"},
		"email":       {"anna@example.com"},
		"city":        {"Riga"},
	}
	postReq := httptest.NewRequest(http.MethodPost, "/settings/profiles/new", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postRec := httptest.NewRecorder()
	s.handleSettingsProfileNew(postRec, postReq)

	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("POST: expected 303 redirect, got %d: %s", postRec.Code, postRec.Body.String())
	}
	if loc := postRec.Header().Get("Location"); loc != "/settings" {
		t.Errorf("expected redirect to /settings, got %q", loc)
	}

	profiles := s.getConfig().GetProfiles()
	if len(profiles) != 2 {
		t.Fatalf("expected 2 profiles (seeded default + new), got %d: %+v", len(profiles), profiles)
	}
	if profiles[0].ID != "default" {
		t.Errorf("expected legacy profile to be seeded as %q, got %q", "default", profiles[0].ID)
	}
	if profiles[1].ID != "anna-popena" {
		t.Errorf("expected new profile ID %q, got %q", "anna-popena", profiles[1].ID)
	}
	if profiles[1].Email != "anna@example.com" || profiles[1].City != "Riga" || profiles[1].MiddleName != "Marija" {
		t.Errorf("new profile fields not saved correctly: %+v", profiles[1])
	}
}

func TestHandleSettingsProfileNewValidatesRequiredFields(t *testing.T) {
	s := newTestServer(t, testConfig())
	s.configPath = filepath.Join(t.TempDir(), "config.yaml")

	form := url.Values{"first_name": {"Anna"}} // missing last_name and email
	req := httptest.NewRequest(http.MethodPost, "/settings/profiles/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleSettingsProfileNew(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (re-render with errors), got %d", rec.Code)
	}
	if len(s.getConfig().GetProfiles()) != 1 {
		t.Error("no profile should have been added when validation fails")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Last name is required") || !strings.Contains(body, "Email is required") {
		t.Errorf("expected validation error messages in response body, got: %s", body)
	}
}

func TestHandleSettingsProfileEditUpdatesFields(t *testing.T) {
	s := newTestServer(t, testConfig("default", "spouse"))
	s.configPath = filepath.Join(t.TempDir(), "config.yaml")

	// GET should pre-fill the form from the existing profile, not a blank one.
	getReq := withURLParam(httptest.NewRequest(http.MethodGet, "/settings/profiles/spouse/edit", nil), "profileID", "spouse")
	getRec := httptest.NewRecorder()
	s.handleSettingsProfileEdit(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", getRec.Code, getRec.Body.String())
	}
	if !strings.Contains(getRec.Body.String(), "spouse@example.com") {
		t.Errorf("expected GET form to be pre-filled with the existing profile's email, body: %s", getRec.Body.String())
	}

	form := url.Values{
		"first_name": {"Updated"},
		"last_name":  {"Name"},
		"email":      {"updated@example.com"},
		"city":       {"Vilnius"},
	}
	postReq := withURLParam(httptest.NewRequest(http.MethodPost, "/settings/profiles/spouse/edit", strings.NewReader(form.Encode())), "profileID", "spouse")
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postRec := httptest.NewRecorder()
	s.handleSettingsProfileEdit(postRec, postReq)

	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("POST: expected 303 redirect, got %d: %s", postRec.Code, postRec.Body.String())
	}

	profiles := s.getConfig().GetProfiles()
	if len(profiles) != 2 {
		t.Fatalf("expected profile count to stay 2, got %d: %+v", len(profiles), profiles)
	}
	if profiles[0].ID != "default" {
		t.Errorf("expected first profile to remain %q untouched, got %+v", "default", profiles[0])
	}
	if profiles[1].ID != "spouse" {
		t.Errorf("expected edited profile's ID to stay %q, got %q", "spouse", profiles[1].ID)
	}
	if profiles[1].FirstName != "Updated" || profiles[1].Email != "updated@example.com" || profiles[1].City != "Vilnius" {
		t.Errorf("edited profile fields not saved correctly: %+v", profiles[1])
	}
}

func TestHandleSettingsProfileEditUnknownIDReturns404(t *testing.T) {
	s := newTestServer(t, testConfig("default"))
	s.configPath = filepath.Join(t.TempDir(), "config.yaml")

	req := withURLParam(httptest.NewRequest(http.MethodGet, "/settings/profiles/nonexistent/edit", nil), "profileID", "nonexistent")
	rec := httptest.NewRecorder()
	s.handleSettingsProfileEdit(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown profile ID, got %d", rec.Code)
	}
}

func TestHandleSettingsProfileEditLegacySingleProfileWritesBackToProfileBlock(t *testing.T) {
	// No profiles: list configured - just the legacy top-level profile:
	// block, synthesized as the "default" profile by GetProfiles().
	s := newTestServer(t, testConfig())
	s.configPath = filepath.Join(t.TempDir(), "config.yaml")

	form := url.Values{
		"first_name": {"New"},
		"last_name":  {"Name"},
		"email":      {"new@example.com"},
	}
	req := withURLParam(httptest.NewRequest(http.MethodPost, "/settings/profiles/default/edit", strings.NewReader(form.Encode())), "profileID", "default")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleSettingsProfileEdit(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d: %s", rec.Code, rec.Body.String())
	}

	cfg := s.getConfig()
	if len(cfg.Profiles) != 0 {
		t.Errorf("expected editing the legacy default profile to stay in single-profile mode (no profiles: list), got %+v", cfg.Profiles)
	}
	if cfg.Profile.FirstName != "New" || cfg.Profile.Email != "new@example.com" {
		t.Errorf("expected legacy profile: block to be updated, got %+v", cfg.Profile)
	}
}

func TestHandleSettingsProfileDeleteRemovesProfile(t *testing.T) {
	s := newTestServer(t, testConfig("default", "spouse"))
	s.configPath = filepath.Join(t.TempDir(), "config.yaml")

	req := withURLParam(httptest.NewRequest(http.MethodPost, "/settings/profiles/spouse/delete", nil), "profileID", "spouse")
	rec := httptest.NewRecorder()
	s.handleSettingsProfileDelete(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d: %s", rec.Code, rec.Body.String())
	}

	profiles := s.getConfig().GetProfiles()
	if len(profiles) != 1 || profiles[0].ID != "default" {
		t.Errorf("expected only %q to remain, got %+v", "default", profiles)
	}
}

func TestHandleSettingsProfileDeleteRefusesToRemoveLastProfile(t *testing.T) {
	s := newTestServer(t, testConfig("default"))
	s.configPath = filepath.Join(t.TempDir(), "config.yaml")

	req := withURLParam(httptest.NewRequest(http.MethodPost, "/settings/profiles/default/delete", nil), "profileID", "default")
	rec := httptest.NewRecorder()
	s.handleSettingsProfileDelete(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 when deleting the only profile, got %d", rec.Code)
	}
	if len(s.getConfig().GetProfiles()) != 1 {
		t.Error("the only profile should not have been removed")
	}
}

func TestHandleSettingsProfileDeleteClearsActiveProfileCookie(t *testing.T) {
	s := newTestServer(t, testConfig("default", "spouse"))
	s.configPath = filepath.Join(t.TempDir(), "config.yaml")

	req := withURLParam(httptest.NewRequest(http.MethodPost, "/settings/profiles/spouse/delete", nil), "profileID", "spouse")
	req.AddCookie(&http.Cookie{Name: activeProfileCookie, Value: "spouse"})
	rec := httptest.NewRecorder()
	s.handleSettingsProfileDelete(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d: %s", rec.Code, rec.Body.String())
	}

	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == activeProfileCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("expected the active-profile cookie to be cleared after deleting the profile it pointed to")
	}
}

// ==================== Profile list fields ====================

func TestSplitLines(t *testing.T) {
	cases := map[string]struct {
		in   string
		want []string
	}{
		"one per line":           {"a\nb\nc", []string{"a", "b", "c"}},
		"windows line endings":   {"a\r\nb\r\n", []string{"a", "b"}},
		"blank lines discarded":  {"a\n\n\nb\n", []string{"a", "b"}},
		"surrounding whitespace": {"  a  \n\tb\t\n", []string{"a", "b"}},
		"entries containing commas": {"12 Main St, Apt 4, Springfield\n9 Other Rd, Riga",
			[]string{"12 Main St, Apt 4, Springfield", "9 Other Rd, Riga"}},
		"empty input":     {"", nil},
		"whitespace only": {"   \n\t\n  ", nil},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := splitLines(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("entry %d: got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestProfileEditPreservesFieldsWithNoFormControl is the regression test for
// the data-loss bug. buildProfileFromForm constructs a fresh config.Profile,
// so every field the form doesn't carry was reset to its zero value on save:
// editing a profile in the web UI silently erased name_variants,
// additional_phones and date_of_birth. Those can only be set through
// `eraser init`, and they exist precisely because brokers index people under
// old identities - losing them makes removal requests less likely to match.
func TestProfileEditPreservesFieldsWithNoFormControl(t *testing.T) {
	cfg := testConfig()
	cfg.Profile.NameVariants = []string{"Maris Ozolins", "M. Ozolins"}
	cfg.Profile.AdditionalPhones = []string{"+371 20000000"}
	cfg.Profile.DateOfBirth = "1985-03-14"

	s := newTestServer(t, cfg)
	s.configPath = filepath.Join(t.TempDir(), "config.yaml")

	form := url.Values{
		"first_name": {"Test"},
		"last_name":  {"User"},
		"email":      {"test@example.com"},
	}
	req := withURLParam(
		httptest.NewRequest(http.MethodPost, "/settings/profiles/default/edit", strings.NewReader(form.Encode())),
		"profileID", "default")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleSettingsProfileEdit(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d: %s", rec.Code, rec.Body.String())
	}

	saved := s.getConfig().Profile
	if len(saved.NameVariants) != 2 {
		t.Errorf("name_variants was wiped by an edit: %q", saved.NameVariants)
	}
	if len(saved.AdditionalPhones) != 1 {
		t.Errorf("additional_phones was wiped by an edit: %q", saved.AdditionalPhones)
	}
	if saved.DateOfBirth != "1985-03-14" {
		t.Errorf("date_of_birth was wiped by an edit: %q", saved.DateOfBirth)
	}
}

// TestProfileEditRoundTripsListFields covers the enhancement: the two list
// fields with real value for matching are now editable, so an edit must be
// able to add entries, change them, and remove them - not merely leave them
// alone.
func TestProfileEditRoundTripsListFields(t *testing.T) {
	cfg := testConfig()
	cfg.Profile.PreviousAddresses = []string{"1 Old Street, Springfield"}
	cfg.Profile.AdditionalEmails = []string{"stale@example.com"}

	s := newTestServer(t, cfg)
	s.configPath = filepath.Join(t.TempDir(), "config.yaml")

	submit := func(t *testing.T, addresses, emails string) config.Profile {
		t.Helper()
		form := url.Values{
			"first_name":         {"Test"},
			"last_name":          {"User"},
			"email":              {"test@example.com"},
			"previous_addresses": {addresses},
			"additional_emails":  {emails},
		}
		req := withURLParam(
			httptest.NewRequest(http.MethodPost, "/settings/profiles/default/edit", strings.NewReader(form.Encode())),
			"profileID", "default")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		s.handleSettingsProfileEdit(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("expected 303, got %d: %s", rec.Code, rec.Body.String())
		}
		return s.getConfig().Profile
	}

	t.Run("add and edit entries", func(t *testing.T) {
		saved := submit(t,
			"1 Old Street, Springfield\n2 Newer Road, Apt 5, Riga",
			"stale@example.com\nwork@example.com")

		if len(saved.PreviousAddresses) != 2 {
			t.Fatalf("previous_addresses = %q, want 2 entries", saved.PreviousAddresses)
		}
		// Commas within an entry must not split it.
		if saved.PreviousAddresses[1] != "2 Newer Road, Apt 5, Riga" {
			t.Errorf("an address containing commas was split: %q", saved.PreviousAddresses[1])
		}
		if len(saved.AdditionalEmails) != 2 {
			t.Errorf("additional_emails = %q, want 2 entries", saved.AdditionalEmails)
		}
	})

	t.Run("remove entries", func(t *testing.T) {
		saved := submit(t, "1 Old Street, Springfield", "")

		if len(saved.PreviousAddresses) != 1 {
			t.Errorf("previous_addresses = %q, want 1 entry after removing one", saved.PreviousAddresses)
		}
		if len(saved.AdditionalEmails) != 0 {
			t.Errorf("clearing the textarea left %q behind", saved.AdditionalEmails)
		}
	})
}

// TestProfileEditFormShowsExistingListEntries covers the other half of
// "editable": the values have to be rendered back into the textareas, or a
// save would silently wipe whatever the user couldn't see.
func TestProfileEditFormShowsExistingListEntries(t *testing.T) {
	cfg := testConfig()
	cfg.Profile.PreviousAddresses = []string{"1 Old Street, Springfield", "2 Newer Road, Riga"}
	cfg.Profile.AdditionalEmails = []string{"old@example.com"}

	s := newTestServer(t, cfg)
	req := withURLParam(httptest.NewRequest(http.MethodGet, "/settings/profiles/default/edit", nil), "profileID", "default")
	rec := httptest.NewRecorder()
	s.handleSettingsProfileEdit(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`name="previous_addresses"`,
		`name="additional_emails"`,
		"old@example.com",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the edit form does not show %q", want)
		}
	}

	// Assert the exact textarea contents, separator included. A substring
	// check would pass on any separator, but the separator is load-bearing:
	// splitLines reads these back on the next save by splitting on newlines,
	// so rendering them comma-joined would silently collapse every entry
	// into one on the very next save.
	wantAddresses := "1 Old Street, Springfield\n2 Newer Road, Riga"
	if !strings.Contains(body, ">"+wantAddresses+"</textarea>") {
		t.Errorf("previous addresses are not rendered one per line; want a textarea containing %q", wantAddresses)
	}
}
