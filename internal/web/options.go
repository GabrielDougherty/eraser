package web

import (
	"github.com/eraser-privacy/eraser/internal/config"
	"github.com/eraser-privacy/eraser/internal/email"
)

// Option configures a Server at construction time. Options exist rather than
// exported setters because the values they set (the sender factory in
// particular) are read from the background goroutines processSendJob spawns -
// a setter callable after Start() would be a data race waiting to happen.
type Option func(*Server)

// WithSenderFactory replaces how the server builds email senders, covering
// every send path at once: the bulk job, single-broker sends, the auto-resumed
// pending job, and the setup wizard's test send.
//
// This has to be an injected factory rather than anything read from the config
// file, because completing the setup wizard writes a brand new config.yaml
// over the top of whatever was there - so a config-borne switch would be
// erased partway through a from-scratch run. The wizard's test send is the
// clinching case: it builds its own EmailConfig from the in-progress session
// with the provider hardcoded, so no config value can reach it at all.
func WithSenderFactory(f email.Factory) Option {
	return func(s *Server) {
		s.senderFactory = f
	}
}

// WithCaptureSender puts the server in capture mode: all sends are recorded to
// the sender's directory and none are transmitted. One CaptureSender is shared
// across every path so the sequence numbers and manifest give a single ordered
// record of the whole run.
//
// It also records the directory for display, so the UI can show that this
// server isn't really sending. That matters more than it looks: the worst
// failure mode of capture mode is silence in either direction - a test suite
// that believes it is capturing while actually talking to a real mail server,
// or a user who thinks mail went out when it did not.
func WithCaptureSender(cs *email.CaptureSender) Option {
	return func(s *Server) {
		s.senderFactory = func(config.EmailConfig) (email.Sender, error) { return cs, nil }
		s.captureDir = cs.Dir()
	}
}

// WithNoBrowser stops Start from opening the user's browser. Wanted by
// anything that runs the server unattended: headless end-to-end tests, and
// people running `eraser serve` over SSH or in a tmux pane where launching a
// browser on the server host is useless at best.
func WithNoBrowser() Option {
	return func(s *Server) {
		s.noBrowser = true
	}
}
