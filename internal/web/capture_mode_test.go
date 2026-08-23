package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eraser-privacy/eraser/internal/broker"
	"github.com/eraser-privacy/eraser/internal/config"
	"github.com/eraser-privacy/eraser/internal/email"
)

// newCaptureTestServer builds a server in capture mode plus the sender, so a
// test can assert on what was recorded.
func newCaptureTestServer(t *testing.T, cfg *config.Config) (*Server, *email.CaptureSender) {
	t.Helper()

	cs, err := email.NewCaptureSender(filepath.Join(t.TempDir(), "captured"))
	if err != nil {
		t.Fatalf("NewCaptureSender: %v", err)
	}
	return newTestServer(t, cfg, WithCaptureSender(cs)), cs
}

func captureTestConfig() *config.Config {
	cfg := testConfig()
	cfg.Email = config.EmailConfig{
		Provider: "smtp",
		From:     "me@example.com",
		SMTP: config.SMTPConfig{
			// Deliberately unreachable. If interception ever breaks, these
			// tests fail on a connection error rather than quietly sending.
			Host: "smtp.invalid", Port: 465, Username: "me@example.com",
			Password: "nope", UseTLS: true,
		},
	}
	cfg.Options.Template = "generic"
	return cfg
}

// TestCaptureModeInterceptsSingleSend covers handleAPISendOne, which builds
// its sender from the on-disk config.
func TestCaptureModeInterceptsSingleSend(t *testing.T) {
	s, cs := newCaptureTestServer(t, captureTestConfig())
	s.brokerDB = &broker.BrokerDatabase{Brokers: []broker.Broker{
		{ID: "alpha", Name: "Alpha Data", Email: "privacy@alpha.invalid"},
	}}

	req := withURLParam(httptest.NewRequest(http.MethodPost, "/api/send/alpha", nil), "brokerID", "alpha")
	rec := httptest.NewRecorder()
	s.handleAPISendOne(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	msgs := cs.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 captured message, got %d", len(msgs))
	}
	if msgs[0].To != "privacy@alpha.invalid" {
		t.Errorf("captured recipient = %q, want %q", msgs[0].To, "privacy@alpha.invalid")
	}
}

// TestCaptureModeInterceptsWizardTestSend is the assertion that justifies the
// whole factory design. handleSetupTestSend builds its EmailConfig from the
// in-progress wizard session with the provider hardcoded to smtp - it never
// consults the on-disk config - so no config field or provider value could
// intercept it. Only a factory held by the Server reaches this path.
func TestCaptureModeInterceptsWizardTestSend(t *testing.T) {
	s, cs := newCaptureTestServer(t, captureTestConfig())

	// Walk the wizard far enough to have a session with a profile and email.
	form := url.Values{
		"first_name": {"Test"}, "last_name": {"User"}, "email": {"someone@example.com"},
	}
	profileReq := httptest.NewRequest(http.MethodPost, "/setup/profile", strings.NewReader(form.Encode()))
	profileReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	profileRec := httptest.NewRecorder()
	s.handleSetupProfile(profileRec, profileReq)

	var sessionCookie *http.Cookie
	res := profileRec.Result()
	defer func() { _ = res.Body.Close() }()
	for _, c := range res.Cookies() {
		if c.Name == "eraser_session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("no session cookie from the profile step")
	}

	emailForm := url.Values{
		"smtp_host": {"smtp.invalid"}, "smtp_port": {"465"},
		"smtp_username": {"me@example.com"}, "smtp_password": {"nope"}, "smtp_tls": {"on"},
	}
	emailReq := httptest.NewRequest(http.MethodPost, "/setup/email", strings.NewReader(emailForm.Encode()))
	emailReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	emailReq.AddCookie(sessionCookie)
	emailRec := httptest.NewRecorder()
	s.handleSetupEmail(emailRec, emailReq)
	if emailRec.Code != http.StatusFound {
		t.Fatalf("email step: expected 302, got %d: %s", emailRec.Code, emailRec.Body.String())
	}

	// The test send. Without interception this would dial smtp.invalid.
	sendReq := httptest.NewRequest(http.MethodPost, "/setup/test/send", nil)
	sendReq.AddCookie(sessionCookie)
	sendRec := httptest.NewRecorder()
	s.handleSetupTestSend(sendRec, sendReq)

	if body := sendRec.Body.String(); !strings.Contains(body, "Success") {
		t.Fatalf("wizard test send did not succeed, so it was not intercepted: %s", body)
	}

	msgs := cs.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected the wizard test send to be captured, got %d messages", len(msgs))
	}
	if msgs[0].To != "someone@example.com" {
		t.Errorf("captured recipient = %q, want the profile address", msgs[0].To)
	}
	if msgs[0].Subject != "Eraser Test Email" {
		t.Errorf("captured subject = %q", msgs[0].Subject)
	}
}

// TestCaptureModeSharesOneSender checks that both paths land in one ordered
// record. Separate senders per call site would give each its own sequence
// numbering and manifest, making a run impossible to read back in order.
func TestCaptureModeSharesOneSender(t *testing.T) {
	s, cs := newCaptureTestServer(t, captureTestConfig())
	s.brokerDB = &broker.BrokerDatabase{Brokers: []broker.Broker{
		{ID: "alpha", Name: "Alpha Data", Email: "privacy@alpha.invalid"},
		{ID: "bravo", Name: "Bravo Data", Email: "privacy@bravo.invalid"},
	}}

	for _, id := range []string{"alpha", "bravo"} {
		req := withURLParam(httptest.NewRequest(http.MethodPost, "/api/send/"+id, nil), "brokerID", id)
		rec := httptest.NewRecorder()
		s.handleAPISendOne(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("send %s: expected 200, got %d", id, rec.Code)
		}
	}

	msgs := cs.Messages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 captured messages, got %d", len(msgs))
	}
	if msgs[0].Seq != 1 || msgs[1].Seq != 2 {
		t.Errorf("expected one continuous sequence across sends, got %d then %d", msgs[0].Seq, msgs[1].Seq)
	}
}

// TestCaptureDirSurfacedToTemplates pins the UI banner's data source. Capture
// mode failing silently - in either direction - is the worst outcome here.
func TestCaptureDirSurfacedToTemplates(t *testing.T) {
	s, cs := newCaptureTestServer(t, captureTestConfig())
	if s.captureDir != cs.Dir() {
		t.Errorf("captureDir = %q, want %q", s.captureDir, cs.Dir())
	}

	plain := newTestServer(t, captureTestConfig())
	if plain.captureDir != "" {
		t.Errorf("a server sending for real must report no capture dir, got %q", plain.captureDir)
	}
}

// TestDryRunConfigSuppressesWebSends closes a real safety gap: options.dry_run
// was read only by the CLI, so a config carrying dry_run: true still sent for
// real to every broker the moment someone pressed "Send to All" in the web UI.
func TestDryRunConfigSuppressesWebSends(t *testing.T) {
	cfg := captureTestConfig()
	cfg.Options.DryRun = true

	s := newTestServer(t, cfg)
	s.configPath = filepath.Join(t.TempDir(), "config.yaml")
	s.brokerDB = &broker.BrokerDatabase{Brokers: []broker.Broker{
		{ID: "alpha", Name: "Alpha Data", Email: "privacy@alpha.invalid"},
	}}

	req := withURLParam(httptest.NewRequest(http.MethodPost, "/api/send/alpha", nil), "brokerID", "alpha")
	rec := httptest.NewRecorder()
	s.handleAPISendOne(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Nothing may have gone out; the message must be on disk instead. Without
	// the fix this reaches the real SMTP sender and fails against smtp.invalid.
	dir := filepath.Join(filepath.Dir(s.configPath), "dry-run")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("expected dry-run captures at %s: %v", dir, err)
	}
	var emls int
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".eml") {
			emls++
		}
	}
	if emls != 1 {
		t.Errorf("expected 1 recorded message, got %d", emls)
	}

	// And it must say so in the UI rather than suppressing sends silently.
	if s.dryRunCaptureDir() != dir {
		t.Errorf("dryRunCaptureDir() = %q, want %q", s.dryRunCaptureDir(), dir)
	}
}

// TestExplicitCaptureDirWinsOverDryRun pins the precedence: a directory given
// for this run beats a dry_run flag persisted in a config file.
func TestExplicitCaptureDirWinsOverDryRun(t *testing.T) {
	cfg := captureTestConfig()
	cfg.Options.DryRun = true

	s, cs := newCaptureTestServer(t, cfg)
	s.configPath = filepath.Join(t.TempDir(), "config.yaml")
	s.brokerDB = &broker.BrokerDatabase{Brokers: []broker.Broker{
		{ID: "alpha", Name: "Alpha Data", Email: "privacy@alpha.invalid"},
	}}

	req := withURLParam(httptest.NewRequest(http.MethodPost, "/api/send/alpha", nil), "brokerID", "alpha")
	rec := httptest.NewRecorder()
	s.handleAPISendOne(rec, req)

	if got := len(cs.Messages()); got != 1 {
		t.Errorf("expected the explicit capture dir to receive the message, got %d", got)
	}
	if s.dryRunCaptureDir() != "" {
		t.Error("dry-run dir should be inert when an explicit capture dir is set")
	}
}
