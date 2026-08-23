package email

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eraser-privacy/eraser/internal/config"
)

// testSMTPConfig is deliberately pointed at a reserved-for-testing host on an
// unroutable port. The parity test below only exercises validation, which
// SMTPSender applies before it opens any connection, so nothing is dialed.
func testSMTPConfig() config.SMTPConfig {
	return config.SMTPConfig{Host: "smtp.invalid", Port: 465, UseTLS: true}
}

func testMessage() Message {
	return Message{
		To:      "privacy@alpha.invalid",
		From:    "me@example.com",
		Subject: "GDPR Data Erasure Request",
		Body:    "To Whom It May Concern at Alpha,\r\n\r\nPlease erase my data.\r\n",
	}
}

// TestCaptureSenderWritesWireFormat is the assertion that gives captured .eml
// files their meaning: the bytes on disk must be the same bytes SMTPSender
// would hand to the server. Both go through buildRFC822 - if someone
// reintroduces a separate formatter for either path, this fails.
func TestCaptureSenderWritesWireFormat(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCaptureSender(dir)
	if err != nil {
		t.Fatalf("NewCaptureSender: %v", err)
	}

	msg := testMessage()
	if res := c.Send(context.Background(), msg); !res.Success {
		t.Fatalf("send failed: %v", res.Error)
	}

	got, err := os.ReadFile(filepath.Join(dir, "0001-privacy-alpha.invalid.eml"))
	if err != nil {
		t.Fatalf("expected a captured .eml at a sequence-prefixed name: %v", err)
	}
	if want := string(buildRFC822(msg)); string(got) != want {
		t.Errorf("captured bytes differ from the SMTP wire format\n got: %q\nwant: %q", got, want)
	}

	body := string(got)
	for _, want := range []string{
		"From: me@example.com\r\n",
		"To: privacy@alpha.invalid\r\n",
		"MIME-Version: 1.0\r\n",
		"\r\n\r\nTo Whom It May Concern at Alpha,",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("captured message missing %q", want)
		}
	}
}

// TestCaptureSenderValidationParity pins capture mode to rejecting exactly
// what SMTP rejects. If capture accepted a recipient SMTP refuses, an
// end-to-end test asserting "these two brokers were skipped, none failed"
// would silently stop meaning anything.
func TestCaptureSenderValidationParity(t *testing.T) {
	cases := map[string]Message{
		"empty recipient":  {To: "", From: "me@example.com", Subject: "s", Body: "b"},
		"empty sender":     {To: "a@b.invalid", From: "", Subject: "s", Body: "b"},
		"header injection": {To: "a@b.invalid", From: "me@example.com", Subject: "s\r\nBcc: x@y.invalid", Body: "b"},
	}

	dir := t.TempDir()
	c, err := NewCaptureSender(dir)
	if err != nil {
		t.Fatalf("NewCaptureSender: %v", err)
	}
	smtpSender := NewSMTPSender(testSMTPConfig(), "me@example.com")

	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			capRes := c.Send(context.Background(), msg)
			if capRes.Success {
				t.Fatalf("capture accepted a message SMTP would reject")
			}
			smtpRes := smtpSender.Send(context.Background(), msg)
			if smtpRes.Success {
				t.Fatalf("test bug: SMTP was expected to reject this too")
			}
			if capRes.Error.Error() != smtpRes.Error.Error() {
				t.Errorf("error text differs between senders:\ncapture: %v\n   smtp: %v", capRes.Error, smtpRes.Error)
			}
		})
	}

	// A rejected message must leave nothing behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("rejected messages should write no files, found %d", len(entries))
	}
}

// TestCaptureSenderManifest covers what a test harness actually reads:
// one JSON object per line, in send order.
func TestCaptureSenderManifest(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCaptureSender(dir)
	if err != nil {
		t.Fatalf("NewCaptureSender: %v", err)
	}

	recipients := []string{"a@one.invalid", "b@two.invalid", "c@three.invalid"}
	for _, to := range recipients {
		msg := testMessage()
		msg.To = to
		if res := c.Send(context.Background(), msg); !res.Success {
			t.Fatalf("send to %s failed: %v", to, res.Error)
		}
	}

	f, err := os.Open(filepath.Join(dir, "manifest.jsonl"))
	if err != nil {
		t.Fatalf("expected a manifest: %v", err)
	}
	defer func() { _ = f.Close() }()

	var got []CapturedMessage
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var m CapturedMessage
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
			t.Fatalf("manifest line is not valid JSON: %v", err)
		}
		got = append(got, m)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading manifest: %v", err)
	}

	if len(got) != len(recipients) {
		t.Fatalf("expected %d manifest entries, got %d", len(recipients), len(got))
	}
	for i, want := range recipients {
		if got[i].To != want {
			t.Errorf("entry %d: expected recipient %q, got %q", i, want, got[i].To)
		}
		if got[i].Seq != i+1 {
			t.Errorf("entry %d: expected seq %d, got %d", i, i+1, got[i].Seq)
		}
		if _, err := os.Stat(filepath.Join(dir, got[i].File)); err != nil {
			t.Errorf("entry %d names file %q which does not exist", i, got[i].File)
		}
	}

	if inMemory := c.Messages(); len(inMemory) != len(recipients) {
		t.Errorf("Messages() returned %d, expected %d", len(inMemory), len(recipients))
	}
}

// TestCaptureSenderErrorAvoidsAuthKeyword guards a non-obvious coupling:
// processSendJob halts a whole run when a send error contains "auth", so a
// capture-mode I/O failure phrased as e.g. "unauthorized" would stop the job
// behind a misleading authentication message.
func TestCaptureSenderErrorAvoidsAuthKeyword(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCaptureSender(dir)
	if err != nil {
		t.Fatalf("NewCaptureSender: %v", err)
	}

	// Make writes fail by replacing the directory with a file.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	res := c.Send(context.Background(), testMessage())
	if res.Success {
		t.Fatal("expected the send to fail when the capture directory is unwritable")
	}
	if strings.Contains(strings.ToLower(res.Error.Error()), "auth") {
		t.Errorf("capture error must not contain %q or it trips processSendJob's auth circuit breaker: %v", "auth", res.Error)
	}
}

func TestSanitizeForFilename(t *testing.T) {
	cases := map[string]string{
		"privacy@alpha.invalid": "privacy-alpha.invalid",
		"a/b\\c:d":              "a-b-c-d",
		"":                      "unknown",
		"@@@":                   "-",
	}
	for in, want := range cases {
		if got := sanitizeForFilename(in); got != want {
			t.Errorf("sanitizeForFilename(%q) = %q, want %q", in, got, want)
		}
	}

	long := strings.Repeat("x", 200) + "@example.com"
	if got := sanitizeForFilename(long); len(got) > maxRecipientInFilename {
		t.Errorf("expected truncation to %d chars, got %d", maxRecipientInFilename, len(got))
	}
}
