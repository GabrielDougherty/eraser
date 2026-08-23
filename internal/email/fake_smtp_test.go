package email

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eraser-privacy/eraser/internal/config"
)

// ---------------------------------------------------------------------
// Fake SMTP server.
//
// Follows the pattern already used for IMAP in internal/inbox/monitor_test.go:
// a real listener on 127.0.0.1:0 running a scripted conversation, with
// bounded cleanup so a hung handler fails the test rather than the suite.
//
// It advertises no ESMTP extensions, which is what makes the plaintext path
// reachable at all: net/smtp only attempts STARTTLS when the server offers
// it, and skips authentication entirely when passed a nil smtp.Auth.
// ---------------------------------------------------------------------

// fakeSMTP records what a send actually put on the wire.
type fakeSMTP struct {
	addr string

	mu       sync.Mutex
	mailFrom string
	rcptTo   []string
	data     []byte

	// Reply overrides, for exercising rejection paths. Empty means accept.
	rejectAuth     string
	rejectMailFrom string
	rejectRcptTo   string
	rejectData     string
}

func (f *fakeSMTP) delivered() (from string, to []string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mailFrom, append([]string(nil), f.rcptTo...), append([]byte(nil), f.data...)
}

// startFakeSMTP runs the fake on a plaintext listener and returns it.
func startFakeSMTP(t *testing.T, f *fakeSMTP) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return serveFakeSMTP(t, ln, f)
}

// startFakeSMTPTLS runs the fake behind TLS, returning the certificate pool a
// client needs to verify it. Using a real pool rather than
// InsecureSkipVerify keeps chain verification in play, so a broken
// ServerName would fail the test instead of passing quietly.
func startFakeSMTPTLS(t *testing.T, f *fakeSMTP, hostname string) (*fakeSMTP, *x509.CertPool) {
	t.Helper()

	cert, pool := selfSignedCert(t, hostname)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	return serveFakeSMTP(t, ln, f), pool
}

func serveFakeSMTP(t *testing.T, ln net.Listener, f *fakeSMTP) *fakeSMTP {
	t.Helper()
	f.addr = ln.Addr().String()

	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = conn.Close() }()
		done <- f.converse(conn)
	}()

	t.Cleanup(func() {
		_ = ln.Close()
		select {
		case err := <-done:
			if err != nil && !isExpectedSMTPCloseErr(err) {
				t.Errorf("fake SMTP server: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("fake SMTP server did not finish within 2s")
		}
	})
	return f
}

// converse walks one SMTP exchange. Replies are the minimum net/smtp needs:
// 220 greeting, 250 to EHLO advertising nothing, 250 to MAIL/RCPT, 354 then
// 250 around DATA, 221 to QUIT.
func (f *fakeSMTP) converse(conn net.Conn) error {
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)

	if _, err := io.WriteString(conn, "220 fake ESMTP ready\r\n"); err != nil {
		return err
	}

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))

		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			// No extension lines: no STARTTLS offered, no AUTH offered.
			if _, err := io.WriteString(conn, "250 fake\r\n"); err != nil {
				return err
			}

		case strings.HasPrefix(cmd, "AUTH"):
			// Only reached on the TLS path, where SMTPSender always
			// authenticates.
			reply := "235 authenticated"
			if f.rejectAuth != "" {
				reply = f.rejectAuth
			}
			if _, err := io.WriteString(conn, reply+"\r\n"); err != nil {
				return err
			}

		case strings.HasPrefix(cmd, "MAIL FROM"):
			if f.rejectMailFrom != "" {
				if _, err := io.WriteString(conn, f.rejectMailFrom+"\r\n"); err != nil {
					return err
				}
				continue
			}
			f.mu.Lock()
			f.mailFrom = addressFromCommand(strings.TrimSpace(line))
			f.mu.Unlock()
			if _, err := io.WriteString(conn, "250 ok\r\n"); err != nil {
				return err
			}

		case strings.HasPrefix(cmd, "RCPT TO"):
			if f.rejectRcptTo != "" {
				if _, err := io.WriteString(conn, f.rejectRcptTo+"\r\n"); err != nil {
					return err
				}
				continue
			}
			f.mu.Lock()
			f.rcptTo = append(f.rcptTo, addressFromCommand(strings.TrimSpace(line)))
			f.mu.Unlock()
			if _, err := io.WriteString(conn, "250 ok\r\n"); err != nil {
				return err
			}

		case cmd == "DATA":
			if f.rejectData != "" {
				if _, err := io.WriteString(conn, f.rejectData+"\r\n"); err != nil {
					return err
				}
				continue
			}
			if _, err := io.WriteString(conn, "354 go ahead\r\n"); err != nil {
				return err
			}
			body, err := readDATA(br)
			if err != nil {
				return err
			}
			f.mu.Lock()
			f.data = body
			f.mu.Unlock()
			if _, err := io.WriteString(conn, "250 ok\r\n"); err != nil {
				return err
			}

		case cmd == "QUIT":
			_, err := io.WriteString(conn, "221 bye\r\n")
			return err

		case cmd == "RSET", cmd == "NOOP":
			if _, err := io.WriteString(conn, "250 ok\r\n"); err != nil {
				return err
			}

		default:
			if _, err := io.WriteString(conn, "500 unrecognised\r\n"); err != nil {
				return err
			}
		}
	}
}

// readDATA reads until the lone "." terminator, undoing the dot-stuffing
// net/smtp applies so the result is comparable to what was handed to Send.
func readDATA(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if line == ".\r\n" || line == ".\n" {
			return out, nil
		}
		// RFC 5321 §4.5.2: a leading dot on the wire is an escape.
		if strings.HasPrefix(line, "..") {
			line = line[1:]
		}
		out = append(out, line...)
	}
}

func addressFromCommand(line string) string {
	open := strings.Index(line, "<")
	close := strings.LastIndex(line, ">")
	if open == -1 || close == -1 || close < open {
		return ""
	}
	return line[open+1 : close]
}

func isExpectedSMTPCloseErr(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	msg := err.Error()
	// A client that declines our self-signed certificate aborts the
	// handshake, which surfaces here rather than in the client - expected
	// whenever a test is deliberately checking verification happens.
	return strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "tls: bad certificate") ||
		strings.Contains(msg, "remote error")
}

// selfSignedCert mints a certificate valid for hostname, plus a pool that
// trusts it. Generated per run with a long validity so nothing starts
// failing on a future date.
func selfSignedCert(t *testing.T, hostname string) (tls.Certificate, *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: hostname},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{hostname},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing certificate: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// splitHostPort is a small helper so tests can configure SMTPConfig from a
// listener address.
func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parsing port %q: %v", portStr, err)
	}
	return host, port
}

// ==================== Wire-path tests ====================

func plaintextSender(t *testing.T, f *fakeSMTP) *SMTPSender {
	t.Helper()
	host, port := splitHostPort(t, f.addr)
	// No username: SMTPSender only takes the plaintext branch when
	// authentication isn't configured (it refuses to auth without TLS).
	return NewSMTPSender(config.SMTPConfig{Host: host, Port: port, UseTLS: false}, "me@example.com")
}

// TestSendDeliversTheWireFormat is the assertion this whole fake exists for.
// Capture mode records buildRFC822's output and the end-to-end suite asserts
// against those files, but nothing until now proved that the same bytes are
// what an SMTP server actually receives. If the two ever diverge, every
// capture-based assertion quietly stops meaning anything.
func TestSendDeliversTheWireFormat(t *testing.T) {
	f := startFakeSMTP(t, &fakeSMTP{})
	sender := plaintextSender(t, f)

	msg := Message{
		To:      "privacy@broker.invalid",
		From:    "me@example.com",
		Subject: "GDPR Data Erasure Request - Article 17 Right to Erasure",
		Body:    "To Whom It May Concern at Broker,\r\n\r\nPlease erase my data.\r\n",
	}

	res := sender.Send(context.Background(), msg)
	if !res.Success {
		t.Fatalf("send failed: %v", res.Error)
	}

	from, to, data := f.delivered()
	if from != "me@example.com" {
		t.Errorf("MAIL FROM = %q", from)
	}
	if len(to) != 1 || to[0] != "privacy@broker.invalid" {
		t.Errorf("RCPT TO = %v", to)
	}
	if want := string(buildRFC822(msg)); string(data) != want {
		t.Errorf("the bytes on the wire differ from buildRFC822\n got: %q\nwant: %q", data, want)
	}
}

// TestSendDotStuffingRoundTrips covers a body line beginning with a period.
// On the wire that is escaped, and a server or client mishandling it either
// truncates the message at that line or leaves a stray dot in the text a
// broker reads. Removal-request bodies are templated prose, so a line
// starting with "." is entirely possible.
func TestSendDotStuffingRoundTrips(t *testing.T) {
	f := startFakeSMTP(t, &fakeSMTP{})
	sender := plaintextSender(t, f)

	msg := Message{
		To:      "privacy@broker.invalid",
		From:    "me@example.com",
		Subject: "Test",
		Body:    "First line.\r\n.hidden line starting with a dot\r\nLast line.\r\n",
	}

	if res := sender.Send(context.Background(), msg); !res.Success {
		t.Fatalf("send failed: %v", res.Error)
	}

	_, _, data := f.delivered()
	body := string(data)
	if !strings.Contains(body, "\r\n.hidden line starting with a dot\r\n") {
		t.Errorf("dot-stuffed line did not survive: %q", body)
	}
	if !strings.Contains(body, "Last line.") {
		t.Errorf("message was truncated at the dotted line: %q", body)
	}
}

func TestSendSurfacesServerRejections(t *testing.T) {
	cases := map[string]*fakeSMTP{
		"sender rejected":    {rejectMailFrom: "550 sender not allowed"},
		"recipient rejected": {rejectRcptTo: "550 no such user here"},
		"data rejected":      {rejectData: "552 message too large"},
	}

	for name, fake := range cases {
		t.Run(name, func(t *testing.T) {
			f := startFakeSMTP(t, fake)
			sender := plaintextSender(t, f)

			res := sender.Send(context.Background(), Message{
				To: "privacy@broker.invalid", From: "me@example.com",
				Subject: "Test", Body: "body",
			})
			if res.Success {
				t.Fatal("a server rejection was reported as a successful send")
			}
			if res.Error == nil {
				t.Fatal("no error accompanied the failure")
			}
		})
	}
}

// TestSendSanitizesAuthFailure pins the deliberate laundering of SMTP
// failures. The raw text comes from a remote server and is echoed into the
// UI and history; sanitizeSMTPError collapses it to a fixed phrase.
//
// The wording is doubly load-bearing: processSendJob's circuit breaker keys
// off the substring "auth" to decide whether to halt an entire run. Note
// that the keyword comes from sendWithTLS's own wrapper, not from the
// server - a real Gmail rejection reads "Username and Password not
// accepted" and contains no "auth" at all, so the classification depends
// entirely on that wrapper staying in place.
func TestSendSanitizesAuthFailure(t *testing.T) {
	f, pool := startFakeSMTPTLS(t, &fakeSMTP{
		rejectAuth: "535 5.7.8 Username and Password not accepted",
	}, "localhost")
	_, port := splitHostPort(t, f.addr)

	original := tlsDial
	tlsDial = func(network, addr string, cfg *tls.Config) (*tls.Conn, error) {
		verifying := cfg.Clone()
		verifying.RootCAs = pool
		return tls.Dial(network, addr, verifying)
	}
	t.Cleanup(func() { tlsDial = original })

	sender := NewSMTPSender(config.SMTPConfig{
		Host: "localhost", Port: port,
		Username: "me@example.com", Password: "wrong", UseTLS: true,
	}, "me@example.com")

	res := sender.Send(context.Background(), Message{
		To: "privacy@broker.invalid", From: "me@example.com", Subject: "Test", Body: "body",
	})
	if res.Success {
		t.Fatal("expected the send to fail")
	}
	if got := res.Error.Error(); got != "SMTP authentication failed" {
		t.Errorf("error = %q, want the sanitized auth phrase", got)
	}
	if strings.Contains(res.Error.Error(), "Username and Password") {
		t.Error("the server's raw message leaked through")
	}
	// The circuit breaker's contract.
	if !strings.Contains(strings.ToLower(res.Error.Error()), "auth") {
		t.Error("the error no longer contains \"auth\"; processSendJob's circuit breaker would stop recognising auth failures")
	}
}

// TestSendWithTLS exercises the path that normally requires a real
// certificate authority. The dial seam points at an in-process TLS listener
// whose self-signed certificate is in the pool, so chain verification still
// runs for real - a wrong ServerName here fails rather than passes.
func TestSendWithTLS(t *testing.T) {
	f, pool := startFakeSMTPTLS(t, &fakeSMTP{}, "localhost")
	_, port := splitHostPort(t, f.addr)

	original := tlsDial
	tlsDial = func(network, addr string, cfg *tls.Config) (*tls.Conn, error) {
		verifying := cfg.Clone()
		verifying.RootCAs = pool
		return tls.Dial(network, addr, verifying)
	}
	t.Cleanup(func() { tlsDial = original })

	sender := NewSMTPSender(config.SMTPConfig{
		Host: "localhost", Port: port,
		Username: "me@example.com", Password: "app-password", UseTLS: true,
	}, "me@example.com")

	msg := Message{
		To: "privacy@broker.invalid", From: "me@example.com",
		Subject: "GDPR Data Erasure Request", Body: "Please erase my data.\r\n",
	}
	res := sender.Send(context.Background(), msg)
	if !res.Success {
		t.Fatalf("TLS send failed: %v", res.Error)
	}

	from, to, data := f.delivered()
	if from != "me@example.com" || len(to) != 1 || to[0] != "privacy@broker.invalid" {
		t.Errorf("envelope wrong over TLS: from=%q to=%v", from, to)
	}
	if want := string(buildRFC822(msg)); string(data) != want {
		t.Errorf("TLS delivery differs from buildRFC822\n got: %q\nwant: %q", data, want)
	}
}

// TestSendWithTLSRejectsAnUntrustedCertificate confirms verification is
// genuinely happening. Dialling without the pool must fail, or the previous
// test would prove nothing about certificate handling.
func TestSendWithTLSRejectsAnUntrustedCertificate(t *testing.T) {
	f, _ := startFakeSMTPTLS(t, &fakeSMTP{}, "localhost")
	_, port := splitHostPort(t, f.addr)

	sender := NewSMTPSender(config.SMTPConfig{
		Host: "localhost", Port: port,
		Username: "me@example.com", Password: "app-password", UseTLS: true,
	}, "me@example.com")

	res := sender.Send(context.Background(), Message{
		To: "privacy@broker.invalid", From: "me@example.com", Subject: "Test", Body: "body",
	})
	if res.Success {
		t.Fatal("a self-signed certificate was accepted without being trusted")
	}
}

// TestSendRefusesAuthWithoutTLS pins the rule that credentials never travel
// in clear text, whatever the config says.
func TestSendRefusesAuthWithoutTLS(t *testing.T) {
	f := startFakeSMTP(t, &fakeSMTP{})
	host, port := splitHostPort(t, f.addr)

	sender := NewSMTPSender(config.SMTPConfig{
		Host: host, Port: port, Username: "me@example.com", Password: "secret", UseTLS: false,
	}, "me@example.com")

	res := sender.Send(context.Background(), Message{
		To: "privacy@broker.invalid", From: "me@example.com", Subject: "Test", Body: "body",
	})
	if res.Success {
		t.Fatal("credentials were sent over an unencrypted connection")
	}
	if _, _, data := f.delivered(); len(data) != 0 {
		t.Error("a message was delivered despite refusing to authenticate")
	}
}
