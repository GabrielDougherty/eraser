package email

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/smtp"
	"strings"

	"github.com/eraser-privacy/eraser/internal/config"
)

type SMTPSender struct {
	config config.SMTPConfig
	from   string
}

func NewSMTPSender(cfg config.SMTPConfig, from string) *SMTPSender {
	return &SMTPSender{config: cfg, from: from}
}

func (s *SMTPSender) Name() string { return "smtp" }

func (s *SMTPSender) Send(ctx context.Context, msg Message) Result {
	if err := validateHeaders(msg); err != nil {
		return Result{Success: false, Error: err}
	}

	addr := fmt.Sprintf("%s:%d", s.config.Host, s.config.Port)
	raw := buildRFC822(msg)

	auth := smtp.PlainAuth("", s.config.Username, s.config.Password, s.config.Host)

	var err error
	if s.config.UseTLS {
		err = s.sendWithTLS(addr, auth, msg.From, msg.To, raw)
	} else {
		if s.config.Username != "" {
			return Result{Success: false, Error: fmt.Errorf("SMTP auth requires TLS")}
		}
		err = smtp.SendMail(addr, nil, msg.From, []string{msg.To}, raw)
	}
	if err != nil {
		return Result{Success: false, Error: sanitizeSMTPError(err)}
	}

	return Result{
		Success:   true,
		MessageID: fmt.Sprintf("smtp-%s-%d", msg.To, ctx.Value(SequenceKey)),
	}
}

func sanitizeSMTPError(err error) error {
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "auth") {
		return fmt.Errorf("SMTP authentication failed")
	}
	if strings.Contains(s, "certificate") {
		return fmt.Errorf("TLS certificate error")
	}
	return fmt.Errorf("SMTP error: check your configuration")
}

// tlsDial is the TLS dialer sendWithTLS uses. It exists so tests can point
// the connection at an in-process server holding a self-signed certificate,
// which is otherwise impossible: the config below verifies against the
// system trust store, so no fake and no container can be reached without it.
//
// Deliberately unexported and never assigned outside tests - this must not
// be able to become a "skip certificate checks" switch reachable from
// config or a flag in a tool that carries a live mail credential. The
// stdlib uses the same idiom (net/smtp's testHookStartTLS).
var tlsDial = tls.Dial

func (s *SMTPSender) sendWithTLS(addr string, auth smtp.Auth, from, to string, msg []byte) error {
	conn, err := tlsDial("tcp", addr, &tls.Config{
		ServerName: s.config.Host,
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		return fmt.Errorf("TLS connection failed: %w", err)
	}
	defer func() { _ = conn.Close() }()

	client, err := smtp.NewClient(conn, s.config.Host)
	if err != nil {
		return fmt.Errorf("SMTP client creation failed: %w", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Auth(auth); err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("sender rejected: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("recipient rejected: %w", err)
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("data command failed: %w", err)
	}
	if _, err = w.Write(msg); err != nil {
		return fmt.Errorf("message write failed: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("message finalization failed: %w", err)
	}
	return client.Quit()
}
