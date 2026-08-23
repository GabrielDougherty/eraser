package web

// Option configures a Server at construction time. Options exist rather than
// exported setters because the values they set (the sender factory in
// particular) are read from the background goroutines processSendJob spawns -
// a setter callable after Start() would be a data race waiting to happen.
type Option func(*Server)

// WithNoBrowser stops Start from opening the user's browser. Wanted by
// anything that runs the server unattended: headless end-to-end tests, and
// people running `eraser serve` over SSH or in a tmux pane where launching a
// browser on the server host is useless at best.
func WithNoBrowser() Option {
	return func(s *Server) {
		s.noBrowser = true
	}
}
