package email

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// unsafeFilenameChars matches every run of characters not safe in a filename
// on any of the platforms this runs on. Compiled once at package level rather
// than per-send.
var unsafeFilenameChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// maxRecipientInFilename caps the recipient portion of a captured filename;
// the sequence prefix is what actually guarantees uniqueness.
const maxRecipientInFilename = 64

// errCaptureWrite is returned for any failure to record a message. It is a
// fixed string carrying no path, filename or address, for the same reason
// sanitizeSMTPError collapses SMTP failures: processSendJob halts a whole run
// when a send error contains "auth" (see handlers_jobs.go), and a wrapped
// os error would embed the capture directory and the recipient-derived
// filename - either of which can legitimately contain that substring. A
// broker at privacy@authoritydata.com is enough to trip it.
var errCaptureWrite = errors.New("could not record message to the capture directory")

// CapturedMessage is one recorded send, mirrored into manifest.jsonl so a test
// harness can read structured results without parsing MIME.
type CapturedMessage struct {
	Seq       int       `json:"seq"`
	To        string    `json:"to"`
	From      string    `json:"from"`
	Subject   string    `json:"subject"`
	File      string    `json:"file"`
	MessageID string    `json:"message_id"`
	SentAt    time.Time `json:"sent_at"`
}

// CaptureSender implements Sender by writing what would have been transmitted
// to a directory instead of connecting to an SMTP server. Every other part of
// the pipeline - template rendering, history records, job progress, the
// pipeline state machine - behaves exactly as in a real send, so an end-to-end
// test exercises the real code paths while nothing leaves the machine.
//
// It applies the same validation as SMTPSender (see validateHeaders): a
// recipient SMTP would reject is rejected here too, so tests that assert on
// which brokers were skipped or failed stay meaningful.
type CaptureSender struct {
	mu   sync.Mutex
	dir  string
	seq  int
	msgs []CapturedMessage
}

// NewCaptureSender creates the capture directory up front so a bad path fails
// at startup rather than midway through a send run.
func NewCaptureSender(dir string) (*CaptureSender, error) {
	if dir == "" {
		return nil, fmt.Errorf("capture directory cannot be empty")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create capture directory: %w", err)
	}
	return &CaptureSender{dir: dir}, nil
}

func (c *CaptureSender) Name() string { return "capture" }

// Dir returns the directory messages are written to.
func (c *CaptureSender) Dir() string { return c.dir }

// Messages returns a copy of everything captured so far, for in-process
// assertions that don't want to read the directory back.
func (c *CaptureSender) Messages() []CapturedMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]CapturedMessage(nil), c.msgs...)
}

func (c *CaptureSender) Send(_ context.Context, msg Message) Result {
	if err := validateHeaders(msg); err != nil {
		return Result{Success: false, Error: err}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.seq++
	seq := c.seq
	name := fmt.Sprintf("%04d-%s.eml", seq, sanitizeForFilename(msg.To))

	// Whole-file write, so a harness polling this directory can never observe
	// a partially written message.
	if err := os.WriteFile(filepath.Join(c.dir, name), buildRFC822(msg), 0600); err != nil {
		c.seq--
		return Result{Success: false, Error: errCaptureWrite}
	}

	captured := CapturedMessage{
		Seq:       seq,
		To:        msg.To,
		From:      msg.From,
		Subject:   msg.Subject,
		File:      name,
		MessageID: fmt.Sprintf("capture-%d", seq),
		SentAt:    time.Now(),
	}
	c.msgs = append(c.msgs, captured)

	if err := c.appendManifest(captured); err != nil {
		return Result{Success: false, Error: errCaptureWrite}
	}

	return Result{Success: true, MessageID: captured.MessageID}
}

// appendManifest records one line of JSON per captured message. Caller must
// hold c.mu.
func (c *CaptureSender) appendManifest(m CapturedMessage) error {
	line, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("could not encode manifest entry: %w", err)
	}

	f, err := os.OpenFile(filepath.Join(c.dir, "manifest.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("could not open capture manifest: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("could not write capture manifest: %w", err)
	}
	return nil
}

// sanitizeForFilename makes a recipient address usable as a filename. It is
// only for human readability when browsing the capture directory - the
// sequence number prefix is what keeps names unique, which matters because
// many brokers in the database share one parent company's privacy address.
func sanitizeForFilename(s string) string {
	s = unsafeFilenameChars.ReplaceAllString(s, "-")
	if len(s) > maxRecipientInFilename {
		s = s[:maxRecipientInFilename]
	}
	if s == "" {
		return "unknown"
	}
	return s
}
