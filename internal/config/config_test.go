package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}
	return path
}

const minimalConfig = `
profile:
  first_name: Test
  last_name: User
  email: test@example.com
email:
  provider: smtp
  from: test@example.com
  smtp:
    host: smtp.example.com
    port: 465
`

func TestLoadPreservesExplicitBrowserHeadlessFalse(t *testing.T) {
	path := writeTestConfig(t, minimalConfig+"pipeline:\n  browser_headless: false\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pipeline.Headless() != false {
		t.Errorf("expected Headless() to stay false when explicitly set, got true")
	}

	// Round-trip through Save to make sure re-saving doesn't lose it either -
	// this is what `eraser init` now does on every update-mode run.
	savedPath := filepath.Join(t.TempDir(), "resaved.yaml")
	if err := Save(savedPath, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load(savedPath)
	if err != nil {
		t.Fatalf("reload after save: %v", err)
	}
	if reloaded.Pipeline.Headless() != false {
		t.Errorf("expected Headless() to survive a save/reload round-trip, got true")
	}
}

func TestLoadDefaultsBrowserHeadlessTrueWhenUnset(t *testing.T) {
	path := writeTestConfig(t, minimalConfig)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pipeline.Headless() != true {
		t.Errorf("expected Headless() to default to true when unset, got false")
	}
	if cfg.Pipeline.BrowserHeadless != nil {
		t.Errorf("expected BrowserHeadless to stay nil when unset, got %v", *cfg.Pipeline.BrowserHeadless)
	}
}

func TestLoadPreservesExplicitBrowserHeadlessTrue(t *testing.T) {
	path := writeTestConfig(t, minimalConfig+"pipeline:\n  browser_headless: true\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pipeline.Headless() != true {
		t.Errorf("expected Headless() to stay true when explicitly set, got false")
	}
	if cfg.Pipeline.BrowserHeadless == nil || !*cfg.Pipeline.BrowserHeadless {
		t.Errorf("expected BrowserHeadless to be a non-nil true, got %v", cfg.Pipeline.BrowserHeadless)
	}
}

func TestSlugifyProfileID(t *testing.T) {
	tests := []struct {
		name     string
		first    string
		last     string
		existing []NamedProfile
		want     string
	}{
		{"basic", "Jane", "Doe", nil, "jane-doe"},
		{"diacritics and case", "Māris", "Popēns", nil, "m-ris-pop-ns"},
		{"collision appends -2", "Jane", "Doe", []NamedProfile{{ID: "jane-doe"}}, "jane-doe-2"},
		{"collision is case-insensitive", "Jane", "Doe", []NamedProfile{{ID: "JANE-DOE"}}, "jane-doe-2"},
		{"multiple collisions increment", "Jane", "Doe", []NamedProfile{{ID: "jane-doe"}, {ID: "jane-doe-2"}}, "jane-doe-3"},
		{"empty name falls back", "", "", nil, "profile"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SlugifyProfileID(tt.first, tt.last, tt.existing)
			if got != tt.want {
				t.Errorf("SlugifyProfileID(%q, %q, %v) = %q, want %q", tt.first, tt.last, tt.existing, got, tt.want)
			}
		})
	}
}

func TestSlugifyID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "spouse", "spouse"},
		{"spaces and punctuation", "María López!", "mar-a-l-pez"},
		{"already valid", "kid1", "kid1"},
		{"empty falls back", "", "profile"},
		{"only symbols falls back", "!!!", "profile"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SlugifyID(tt.in)
			if got != tt.want {
				t.Errorf("SlugifyID(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// ==================== Profile resolution ====================
//
// GetProfiles and GetProfile decide *whose* removal requests get sent and
// whose send history is read. Resolving to the wrong profile means emailing
// brokers on behalf of one household member using another's identity, and
// recording it against the wrong history. The web UI's profile switcher, the
// --profile flag, and every history query all funnel through here.

func TestGetProfilesWrapsTheLegacyBlock(t *testing.T) {
	// A config predating multi-profile support has a single top-level
	// profile: block and no profiles: list. It must keep working untouched,
	// presented as one profile under the same id that pre-migration history
	// rows were backfilled to.
	cfg := &Config{Profile: Profile{FirstName: "Solo", LastName: "User", Email: "solo@example.com"}}

	profiles := cfg.GetProfiles()
	if len(profiles) != 1 {
		t.Fatalf("expected 1 synthesized profile, got %d", len(profiles))
	}
	if profiles[0].ID != DefaultProfileID {
		t.Errorf("synthesized profile id = %q, want %q; history rows are keyed on this", profiles[0].ID, DefaultProfileID)
	}
	if profiles[0].FirstName != "Solo" || profiles[0].Email != "solo@example.com" {
		t.Errorf("the legacy profile block was not carried over: %+v", profiles[0])
	}
}

func TestGetProfilesPrefersTheListOverTheLegacyBlock(t *testing.T) {
	cfg := &Config{
		Profile: Profile{FirstName: "Legacy", LastName: "Ignored", Email: "legacy@example.com"},
		Profiles: []NamedProfile{
			{ID: "me", Profile: Profile{FirstName: "Me", LastName: "Here", Email: "me@example.com"}},
			{ID: "spouse", Profile: Profile{FirstName: "Spouse", LastName: "Here", Email: "spouse@example.com"}},
		},
	}

	profiles := cfg.GetProfiles()
	if len(profiles) != 2 {
		t.Fatalf("expected the profiles list, got %d entries", len(profiles))
	}
	for _, p := range profiles {
		if p.FirstName == "Legacy" {
			t.Error("the legacy profile block leaked in alongside the profiles list")
		}
	}
}

func TestGetProfileByID(t *testing.T) {
	cfg := &Config{Profiles: []NamedProfile{
		{ID: "me", Profile: Profile{FirstName: "Me", LastName: "Here", Email: "me@example.com"}},
		{ID: "spouse", Profile: Profile{FirstName: "Spouse", LastName: "Here", Email: "spouse@example.com"}},
	}}

	got, err := cfg.GetProfile("spouse")
	if err != nil {
		t.Fatalf("GetProfile(spouse): %v", err)
	}
	if got.FirstName != "Spouse" {
		t.Errorf("resolved to the wrong profile: %+v", got)
	}

	// The web UI's switcher cookie and the --profile flag both carry
	// user-typed text, so matching is case-insensitive.
	upper, err := cfg.GetProfile("SPOUSE")
	if err != nil {
		t.Fatalf("GetProfile(SPOUSE): %v", err)
	}
	if upper.ID != "spouse" {
		t.Errorf("case-insensitive lookup resolved to %q", upper.ID)
	}
}

func TestGetProfileEmptyIDIsAmbiguousOnlyWithSeveral(t *testing.T) {
	single := &Config{Profile: Profile{FirstName: "Solo", LastName: "User", Email: "solo@example.com"}}
	got, err := single.GetProfile("")
	if err != nil {
		t.Fatalf("an empty id must resolve when only one profile exists: %v", err)
	}
	if got.ID != DefaultProfileID {
		t.Errorf("resolved to %q", got.ID)
	}

	// With more than one, guessing would mean silently sending as the wrong
	// person, so this must be an error rather than a default.
	several := &Config{Profiles: []NamedProfile{
		{ID: "me", Profile: Profile{FirstName: "Me", LastName: "Here", Email: "me@example.com"}},
		{ID: "spouse", Profile: Profile{FirstName: "Spouse", LastName: "Here", Email: "spouse@example.com"}},
	}}
	_, err = several.GetProfile("")
	if err == nil {
		t.Fatal("an empty id with several profiles must not silently pick one")
	}
	// The message has to name the choices or the user cannot act on it.
	for _, want := range []string{"me", "spouse", "--profile"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

func TestGetProfileUnknownIDListsTheAvailableOnes(t *testing.T) {
	cfg := &Config{Profiles: []NamedProfile{
		{ID: "me", Profile: Profile{FirstName: "Me", LastName: "Here", Email: "me@example.com"}},
		{ID: "spouse", Profile: Profile{FirstName: "Spouse", LastName: "Here", Email: "spouse@example.com"}},
	}}

	_, err := cfg.GetProfile("nobody")
	if err == nil {
		t.Fatal("expected an error for an unknown profile id")
	}
	for _, want := range []string{"nobody", "me", "spouse"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

func TestFullName(t *testing.T) {
	cases := map[string]struct {
		profile Profile
		want    string
	}{
		"without a middle name": {Profile{FirstName: "Ada", LastName: "Lovelace"}, "Ada Lovelace"},
		"with a middle name":    {Profile{FirstName: "Ada", MiddleName: "King", LastName: "Lovelace"}, "Ada King Lovelace"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// This string goes into the body of every removal request, so a
			// stray double space is visible to every broker.
			if got := tc.profile.FullName(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ==================== Validation ====================

func validConfig() *Config {
	return &Config{
		Profile: Profile{FirstName: "Test", LastName: "User", Email: "test@example.com"},
		Email: EmailConfig{
			Provider: "smtp",
			From:     "test@example.com",
			SMTP:     SMTPConfig{Host: "smtp.example.com", Port: 465, UseTLS: true},
		},
	}
}

func TestValidateAcceptsAGoodConfig(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("a complete config was rejected: %v", err)
	}
}

func TestValidateRejectsIncompleteConfigs(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"missing first name": {func(c *Config) { c.Profile.FirstName = "" }, "first_name"},
		"missing last name":  {func(c *Config) { c.Profile.LastName = "" }, "last_name"},
		"missing email":      {func(c *Config) { c.Profile.Email = "" }, "email is required"},
		"missing provider":   {func(c *Config) { c.Email.Provider = "" }, "provider is required"},
		"missing from":       {func(c *Config) { c.Email.From = "" }, "from address is required"},
		"unknown provider":   {func(c *Config) { c.Email.Provider = "sendgrid" }, "unknown provider"},
		"missing smtp host":  {func(c *Config) { c.Email.SMTP.Host = "" }, "host is required"},
		"missing smtp port":  {func(c *Config) { c.Email.SMTP.Port = 0 }, "port is required"},
		"profile without id": {func(c *Config) { c.Profiles = []NamedProfile{{ID: "", Profile: c.Profile}} }, "needs an id"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should mention %q, got: %v", tc.want, err)
			}
		})
	}
}

// TestValidateRejectsDuplicateProfileIDs matters because the id is the key
// every history row is written under: two profiles sharing one would silently
// merge their send histories, and GetProfile would only ever return the first.
func TestValidateRejectsDuplicateProfileIDs(t *testing.T) {
	cfg := validConfig()
	person := Profile{FirstName: "A", LastName: "B", Email: "a@example.com"}
	cfg.Profiles = []NamedProfile{{ID: "me", Profile: person}, {ID: "ME", Profile: person}}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("duplicate profile ids differing only in case were accepted")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should say duplicate, got: %v", err)
	}
}

func TestValidateInbox(t *testing.T) {
	full := func() *Config {
		c := validConfig()
		c.Inbox = InboxConfig{Enabled: true, Email: "me@example.com", Password: "app-password", Server: "imap.example.com", Port: 993}
		return c
	}

	if err := full().ValidateInbox(); err != nil {
		t.Fatalf("a complete inbox config was rejected: %v", err)
	}

	cases := map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"not enabled":      {func(c *Config) { c.Inbox.Enabled = false }, "not enabled"},
		"missing email":    {func(c *Config) { c.Inbox.Email = "" }, "email address is required"},
		"missing password": {func(c *Config) { c.Inbox.Password = "" }, "password"},
		"missing server":   {func(c *Config) { c.Inbox.Server = "" }, "IMAP server is required"},
		"missing port":     {func(c *Config) { c.Inbox.Port = 0 }, "IMAP port is required"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := full()
			tc.mutate(cfg)
			err := cfg.ValidateInbox()
			if err == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should mention %q, got: %v", tc.want, err)
			}
		})
	}
}

func TestDefaultConfigPath(t *testing.T) {
	got := DefaultConfigPath()
	if !strings.HasSuffix(got, "config.yaml") {
		t.Errorf("DefaultConfigPath = %q, expected it to end in config.yaml", got)
	}
	if got != "config.yaml" && !strings.Contains(got, ".eraser") {
		t.Errorf("DefaultConfigPath = %q, expected it under .eraser or the documented fallback", got)
	}
}
