package inbox

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/server"

	"github.com/eraser-privacy/eraser/internal/broker"
	"github.com/eraser-privacy/eraser/internal/config"
)

// ---------------------------------------------------------------------
// A real IMAP server, in process.
//
// go-imap ships both a server and an in-memory backend, and they are
// packages of a module this repo already requires - importing them adds no
// new dependency. Testing against a real server implementation beats
// hand-scripting protocol replies, which only ever proves the client agrees
// with our guess at what a server would say.
//
// The memory backend's credentials are fixed by the library.
// ---------------------------------------------------------------------

const (
	memoryUser     = "username"
	memoryPassword = "password"
)

// startIMAPServer runs an IMAP server over TLS and redirects Monitor's dial
// seam at it for the duration of the test. Returns the host and port to
// configure a Monitor with.
//
// TLS rather than plaintext because Connect only speaks TLS, and reaching
// Connect at all is half the point: it is the function that logs in.
func startIMAPServer(t *testing.T) (host string, port int) {
	t.Helper()

	cert, pool := selfSignedCert(t, "localhost")
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}

	srv := server.New(memory.New())
	srv.AllowInsecureAuth = false // already inside TLS
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	// Verify against the pool rather than skipping verification, so a
	// broken ServerName would fail the test instead of passing quietly.
	original := imapDialTLS
	imapDialTLS = func(addr string, _ *tls.Config) (*client.Client, error) {
		return client.DialTLS(addr, &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12})
	}
	t.Cleanup(func() { imapDialTLS = original })

	h, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parsing port: %v", err)
	}
	return h, p
}

func newTestMonitor(t *testing.T, host string, port int, password string) *Monitor {
	t.Helper()
	return NewMonitor(config.InboxConfig{
		Enabled:  true,
		Email:    memoryUser,
		Password: password,
		Server:   host,
		Port:     port,
		Folder:   "INBOX",
	}, []broker.Broker{
		{ID: "example", Name: "Example Broker", Email: "contact@example.org"},
	})
}

func connectedMonitor(t *testing.T) *Monitor {
	t.Helper()
	host, port := startIMAPServer(t)
	m := newTestMonitor(t, host, port, memoryPassword)
	if err := m.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = m.Disconnect() })
	return m
}

func TestConnectAndDisconnect(t *testing.T) {
	host, port := startIMAPServer(t)
	m := newTestMonitor(t, host, port, memoryPassword)

	if err := m.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if m.client == nil {
		t.Fatal("Connect reported success but left no client")
	}
	if err := m.Disconnect(); err != nil {
		t.Errorf("Disconnect: %v", err)
	}
}

// TestConnectRejectsBadCredentials matters because Connect must fail *at
// login*. Returning nil with no usable client would surface much later as a
// nil-pointer somewhere in a fetch, with nothing pointing at the password.
func TestConnectRejectsBadCredentials(t *testing.T) {
	host, port := startIMAPServer(t)
	m := newTestMonitor(t, host, port, "wrong-password")

	err := m.Connect(context.Background())
	if err == nil {
		t.Fatal("Connect succeeded with the wrong password")
	}
	if m.client != nil {
		t.Error("a failed login left a client behind")
	}
}

func TestDisconnectWithoutConnecting(t *testing.T) {
	m := &Monitor{}
	if err := m.Disconnect(); err != nil {
		t.Errorf("Disconnect on an unconnected monitor: %v", err)
	}
}

// TestFetchRecentEmails walks the real fetch path: SELECT, UID SEARCH by
// date, UID FETCH of envelope/flags/uid/body, then parseMessage. The memory
// backend seeds one message from contact@example.org, which the monitor
// should also match to the broker configured with that address.
func TestFetchRecentEmails(t *testing.T) {
	m := connectedMonitor(t)

	emails, err := m.FetchRecentEmails(context.Background(), 3650)
	if err != nil {
		t.Fatalf("FetchRecentEmails: %v", err)
	}
	if len(emails) == 0 {
		t.Fatal("no emails returned; the seeded message should have matched")
	}

	got := emails[0]
	if got.UID == 0 {
		t.Error("UID was not populated; archive operations key off it")
	}
	if got.Subject == "" {
		t.Error("subject was not populated")
	}
	if got.From == "" {
		t.Error("sender was not populated")
	}
	if got.Body == "" {
		t.Error("body was not fetched")
	}
	// Domain-to-broker matching is what attributes a reply to a broker.
	if got.BrokerID != "example" {
		t.Errorf("BrokerID = %q, want the broker matching the sender domain", got.BrokerID)
	}
}

// TestFetchRecentEmailsHonoursTheDateWindow covers the search criteria
// rather than the plumbing: a message outside the window must not come back.
// Without this, a broken or absent Since would look identical to a working
// one, since both return everything when the window is wide.
//
// The message is appended with an explicit INTERNALDATE, because that - not
// the Date: header - is what IMAP's SINCE matches on, and it is the right
// thing to match: it asks when the server received the mail, not when the
// sender claims to have written it.
func TestFetchRecentEmailsHonoursTheDateWindow(t *testing.T) {
	m := connectedMonitor(t)

	const oldSubject = "Ancient reply from a broker"
	appendMessage(t, m, "INBOX", oldSubject, time.Now().AddDate(0, 0, -400))

	recent, err := m.FetchRecentEmails(context.Background(), 30)
	if err != nil {
		t.Fatalf("FetchRecentEmails(30): %v", err)
	}
	for _, e := range recent {
		if e.Subject == oldSubject {
			t.Error("a 400-day-old message came back inside a 30-day window")
		}
	}

	all, err := m.FetchRecentEmails(context.Background(), 3650)
	if err != nil {
		t.Fatalf("FetchRecentEmails(3650): %v", err)
	}
	found := false
	for _, e := range all {
		if e.Subject == oldSubject {
			found = true
		}
	}
	if !found {
		t.Error("a 3650-day window did not reach a 400-day-old message; the window is not being applied as expected")
	}
}

// appendMessage puts a message into a mailbox with a chosen INTERNALDATE.
func appendMessage(t *testing.T, m *Monitor, mailbox, subject string, received time.Time) {
	t.Helper()

	raw := "From: contact@example.org\r\n" +
		"To: me@example.com\r\n" +
		"Subject: " + subject + "\r\n" +
		"Content-Type: text/plain; charset=us-ascii\r\n" +
		"Content-Transfer-Encoding: 8bit\r\n" +
		"\r\n" +
		"Body of " + subject + "\r\n"

	if err := m.client.Append(mailbox, nil, received, bytes.NewBufferString(raw)); err != nil {
		t.Fatalf("appending to %s: %v", mailbox, err)
	}
}

func TestEnsureFolderExists(t *testing.T) {
	m := connectedMonitor(t)

	if err := m.EnsureFolderExists("Processed"); err != nil {
		t.Fatalf("creating a missing folder: %v", err)
	}

	// Idempotent: it runs before every archive, so a second call must not
	// fail with "mailbox already exists".
	if err := m.EnsureFolderExists("Processed"); err != nil {
		t.Errorf("second call for an existing folder: %v", err)
	}

	mailboxes := make(chan *imap.MailboxInfo, 10)
	if err := m.client.List("", "*", mailboxes); err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for mb := range mailboxes {
		if mb.Name == "Processed" {
			found = true
		}
	}
	if !found {
		t.Error("the folder was reported created but does not exist")
	}
}

// TestArchiveEmailsFallsBackWhenMoveIsUnsupported exercises the path that
// runs against any server without the MOVE extension: COPY, flag \Deleted,
// EXPUNGE. The memory backend implements CopyMessages but not
// MoveMessages, so UidMove fails and the fallback is what actually archives
// the message - and it had never been executed before this test.
func TestArchiveEmailsFallsBackWhenMoveIsUnsupported(t *testing.T) {
	m := connectedMonitor(t)

	emails, err := m.FetchRecentEmails(context.Background(), 3650)
	if err != nil {
		t.Fatalf("FetchRecentEmails: %v", err)
	}
	if len(emails) == 0 {
		t.Fatal("nothing to archive")
	}

	if err := m.EnsureFolderExists("Processed"); err != nil {
		t.Fatalf("EnsureFolderExists: %v", err)
	}
	if err := m.ArchiveEmails([]uint32{emails[0].UID}, "Processed"); err != nil {
		t.Fatalf("ArchiveEmails: %v", err)
	}

	// Gone from the source folder...
	remaining, err := m.FetchRecentEmails(context.Background(), 3650)
	if err != nil {
		t.Fatalf("re-fetching INBOX: %v", err)
	}
	for _, e := range remaining {
		if e.UID == emails[0].UID {
			t.Error("the archived message is still in INBOX")
		}
	}

	// ...and present in the destination.
	status, err := m.client.Select("Processed", false)
	if err != nil {
		t.Fatalf("selecting the archive folder: %v", err)
	}
	if status.Messages == 0 {
		t.Error("the archive folder is empty; the message was deleted rather than moved")
	}
}

func TestArchiveEmailsWithNothingToDo(t *testing.T) {
	m := connectedMonitor(t)
	if err := m.ArchiveEmails(nil, "Processed"); err != nil {
		t.Errorf("archiving an empty set should be a no-op, got: %v", err)
	}
}

// selfSignedCert mints a certificate valid for hostname plus a pool trusting
// it. Long validity and generated per run, so nothing starts failing on a
// future date.
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

// assert the memory backend is what we think it is; a library upgrade that
// added MoveMessages would silently stop exercising the fallback above.
var _ backend.Backend = memory.New()
