package inbox

import (
	"strings"
	"testing"
)

// TestCleanURLRejectsPrivateAndLoopbackHosts covers the fix in cleanURL that
// unconditionally rejects URLs pointing at private, loopback, link-local, or
// localhost targets - the last line of defense against a broker-reply email
// (attacker-influenced content) pointing an outbound request at an internal
// service or a cloud metadata endpoint, even when the caller has disabled
// the separate domain-allowlist check.
func TestCleanURLRejectsPrivateAndLoopbackHosts(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		rejects bool
	}{
		// Must be rejected.
		{"cloud metadata endpoint", "http://169.254.169.254/latest/meta-data/", true},
		{"localhost by name", "http://localhost:8080/opt-out", true},
		{"IPv4 loopback", "http://127.0.0.1/opt-out", true},
		{"IPv6 loopback", "http://[::1]/opt-out", true},
		{"private range 10.x", "http://10.0.0.5/opt-out", true},
		{"private range 192.168.x", "http://192.168.1.1/opt-out", true},
		{"private range 172.16.x", "http://172.16.0.1/opt-out", true},
		{"unspecified IPv4", "http://0.0.0.0/opt-out", true},
		{"link-local IPv4", "http://169.254.1.1/opt-out", true},
		{"localhost uppercase", "http://LOCALHOST/opt-out", true},

		// Must NOT be rejected - pass through cleanURL normally.
		{"public domain", "http://example.com/opt-out", false},
		{"public domain https", "https://www.somebroker.com/privacy-request?id=1", false},
		{"public IP", "http://8.8.8.8/opt-out", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cleanURL(tt.rawURL)
			if tt.rejects && got != "" {
				t.Errorf("cleanURL(%q) = %q, want rejected (empty string)", tt.rawURL, got)
			}
			if !tt.rejects && got == "" {
				t.Errorf("cleanURL(%q) = \"\", want it to pass through", tt.rawURL)
			}
		})
	}
}

// TestIsPrivateOrLoopbackHost tests the underlying predicate directly for
// finer-grained coverage of edge cases (bare hostnames as passed after
// url.URL.Hostname() has already stripped any port).
func TestIsPrivateOrLoopbackHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"127.0.0.1", true},
		{"127.0.0.2", true}, // whole 127.0.0.0/8 is loopback
		{"0.0.0.0", true},
		{"::1", true},
		{"10.1.2.3", true},
		{"172.16.5.5", true},
		{"172.31.255.255", true},
		{"192.168.0.1", true},
		{"169.254.169.254", true}, // cloud metadata / link-local
		{"224.0.0.1", true},       // link-local multicast

		{"example.com", false},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"172.32.0.1", false},    // just outside the 172.16/12 private block
		{"internal.corp", false}, // non-literal hostname: no DNS lookup performed
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			if got := isPrivateOrLoopbackHost(tt.host); got != tt.want {
				t.Errorf("isPrivateOrLoopbackHost(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

// ==================== Bounce recipient extraction ====================
//
// ExtractBouncedRecipient decides which broker `eraser cleanup-bounces
// --remove` deletes from the database. Its input is a bounce message, which
// is attacker-influenceable in the sense that anyone who can email you can
// shape one - so extracting the wrong address means deleting the wrong
// broker, and extracting the sender's own address means deleting whichever
// broker happens to carry it.

func bounceEmail(subject, body string) *Email {
	return &Email{Subject: subject, Body: body}
}

func TestExtractBouncedRecipientFromNDRFormats(t *testing.T) {
	cases := map[string]struct {
		email *Email
		want  string
	}{
		"sendmail style": {
			bounceEmail("Returned mail: see transcript for details",
				"The following addresses had permanent fatal errors: privacy@deadbroker.com\n\n(reason: 550 no such user)"),
			"privacy@deadbroker.com",
		},
		"gmail style": {
			bounceEmail("Delivery Status Notification (Failure)",
				"Delivery to the following recipient failed permanently: privacy@gone.example\n"),
			"privacy@gone.example",
		},
		"DSN Final-Recipient header": {
			bounceEmail("Undelivered Mail Returned to Sender",
				"Final-Recipient: rfc822;privacy@closed.example\nAction: failed\nStatus: 5.1.1"),
			"privacy@closed.example",
		},
		"exchange style": {
			bounceEmail("Undeliverable: GDPR Request",
				"Your message could not be delivered to: privacy@exchange.example"),
			"privacy@exchange.example",
		},
		"angle-bracket then failure": {
			bounceEmail("Mail delivery failed",
				"<privacy@bracket.example> : host mx.bracket.example said: 550 rejected"),
			"privacy@bracket.example",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ExtractBouncedRecipient(tc.email); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExtractBouncedRecipientIgnoresSystemAddresses covers the fallback path,
// which is where a wrong answer is most likely: with no recognised NDR
// pattern it takes the first address in the message, so the mailer-daemon
// and the user's own account must be filtered out first. Returning either
// would mean cleanup-bounces deleting a broker that never bounced.
// TestExtractBouncedRecipientPrefersNDRPatternsOverTheFallback proves the
// NDR patterns are load-bearing rather than decorative. The fallback simply
// takes the first address that isn't obviously a system mailbox, so whenever
// a bounce mentions any other address first - a support contact, a postmaster
// note naming a helpdesk - the fallback answers with the wrong one. Only the
// structured patterns get this right, and deleting them must fail a test
// rather than quietly degrade accuracy.
//
// This case was added because mutation testing showed the earlier
// NDR-format tests all still passed with the pattern list removed.
func TestExtractBouncedRecipientPrefersNDRPatternsOverTheFallback(t *testing.T) {
	got := ExtractBouncedRecipient(bounceEmail(
		"Undeliverable: GDPR Erasure Request",
		"Please contact support@mailprovider.example if you believe this is an error.\n\n"+
			"Your message could not be delivered to: privacy@realbroker.example\n",
	))
	if got == "support@mailprovider.example" {
		t.Fatal("returned the support contact - the NDR patterns were bypassed and the fallback answered instead")
	}
	if got != "privacy@realbroker.example" {
		t.Errorf("got %q, want privacy@realbroker.example", got)
	}
}

func TestExtractBouncedRecipientIgnoresSystemAddresses(t *testing.T) {
	got := ExtractBouncedRecipient(bounceEmail(
		"Delivery problem",
		"From: MAILER-DAEMON@mx.google.com\nTo: someone@gmail.com\n"+
			"An error occurred. Contact postmaster@mx.google.com.\n"+
			"The address privacy@realbroker.example could not be reached.",
	))
	if got != "privacy@realbroker.example" {
		t.Errorf("got %q, want the broker address - system addresses must be skipped", got)
	}
}

func TestExtractBouncedRecipientFindsNothing(t *testing.T) {
	cases := map[string]*Email{
		"no addresses at all": bounceEmail("Delivery failed", "Something went wrong, no details."),
		"only system addresses": bounceEmail("Failure",
			"From: mailer-daemon@example.com\nContact no-reply@example.com for help."),
		"completely empty": bounceEmail("", ""),
	}
	for name, email := range cases {
		t.Run(name, func(t *testing.T) {
			// Returning "" is what makes cleanup-bounces skip the message.
			// Anything else here is a broker deleted for no reason.
			if got := ExtractBouncedRecipient(email); got != "" {
				t.Errorf("got %q, want empty", got)
			}
		})
	}
}

func TestExtractBouncedRecipientReadsHTMLBodies(t *testing.T) {
	email := &Email{
		Subject:  "Undeliverable",
		HTMLBody: `<html><body><p>Your message could not be delivered to: privacy@htmlonly.example</p></body></html>`,
	}
	if got := ExtractBouncedRecipient(email); got != "privacy@htmlonly.example" {
		t.Errorf("got %q; the HTML body must be searched too", got)
	}
}

func TestStripHTMLSimple(t *testing.T) {
	got := stripHTMLSimple(`<p>Hello <b>there</b></p><br/><div>friend</div>`)
	for _, want := range []string{"Hello", "there", "friend"} {
		if !strings.Contains(got, want) {
			t.Errorf("stripHTMLSimple dropped %q from its output: %q", want, got)
		}
	}
	if strings.Contains(got, "<") || strings.Contains(got, ">") {
		t.Errorf("tags survived stripping: %q", got)
	}
	// Tags become separators rather than vanishing, so adjacent words don't
	// fuse into one token that then fails to match a bounce pattern.
	if strings.Contains(got, "therefriend") {
		t.Errorf("removing tags fused adjacent words: %q", got)
	}
}

// ==================== HTML URL extraction ====================

func TestExtractURLsFromHTML(t *testing.T) {
	urls := extractURLsFromHTML(`
		<html><body>
			<a href="https://broker.example/confirm?token=abc">Confirm deletion</a>
			<a href="https://broker.example/optout">Opt out form</a>
			<p>Or visit https://broker.example/plain-text-link directly.</p>
		</body></html>`)

	for _, want := range []string{
		"https://broker.example/confirm?token=abc",
		"https://broker.example/optout",
		"https://broker.example/plain-text-link",
	} {
		found := false
		for _, got := range urls {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("did not extract %q; got %v", want, urls)
		}
	}
}

func TestExtractURLsFromHTMLHandlesMalformedMarkup(t *testing.T) {
	// Broker emails are not well-formed documents. Extraction must degrade
	// rather than lose the link, since a dropped confirmation URL means a
	// deletion request silently never gets confirmed.
	urls := extractURLsFromHTML(`<div><a href="https://broker.example/confirm">click<p>unclosed`)
	found := false
	for _, u := range urls {
		if u == "https://broker.example/confirm" {
			found = true
		}
	}
	if !found {
		t.Errorf("lost the href in malformed markup: %v", urls)
	}
}

func TestExtractURLsFromHTMLOnEmptyInput(t *testing.T) {
	if urls := extractURLsFromHTML(""); len(urls) != 0 {
		t.Errorf("expected no URLs from empty HTML, got %v", urls)
	}
}
