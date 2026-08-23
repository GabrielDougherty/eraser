package email

import (
	"fmt"
	"strings"
)

// buildRFC822 renders a Message into the wire format handed to the SMTP
// server. Shared by SMTPSender and CaptureSender so that a captured .eml file
// is byte-for-byte what would have been transmitted - a second, parallel
// implementation of this format would let capture-mode tests pass while real
// sends were malformed.
//
// Deliberately emits no Date or Message-ID header, matching what this has
// always sent. Adding them is a real deliverability improvement and worth
// doing, but it changes every outgoing email and belongs in its own change
// rather than riding along with test infrastructure.
func buildRFC822(msg Message) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", msg.From)
	fmt.Fprintf(&b, "To: %s\r\n", msg.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", msg.Subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(msg.Body)
	return []byte(b.String())
}

// validateHeaders runs the checks every Sender must apply before a message is
// transmitted or recorded: address validity, and a subject that can't smuggle
// extra headers via CRLF. Senders share this so capture mode rejects exactly
// what SMTP rejects - if capture accepted, say, an empty recipient that SMTP
// refuses, tests asserting on which brokers got skipped would be meaningless.
func validateHeaders(msg Message) error {
	if err := validateMessage(msg); err != nil {
		return err
	}
	if strings.ContainsAny(msg.Subject, "\r\n") {
		return fmt.Errorf("subject contains invalid characters")
	}
	return nil
}
