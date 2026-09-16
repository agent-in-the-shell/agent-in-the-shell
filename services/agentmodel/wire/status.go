package wire

// UpstreamStatusHinter is implemented by provider errors that know the HTTP
// status the upstream actually returned. Wrap classifies by that status instead
// of guessing it back out of the error text, so a provider only has to expose
// this method rather than construct a wire.Error itself.
//
// It exists because classification used to substring-match the whole error
// string, and provider adapters interpolate the raw upstream body into that
// string: a genuine 500 whose body happened to contain "401" was reported as an
// authentication failure and — being non-retryable — stopped the router's
// fallback walk. The status is not a heuristic, so it is not guessable wrong.
type UpstreamStatusHinter interface {
	// UpstreamStatus returns the HTTP status the upstream returned, or 0 when
	// the failure never reached one (transport error, cancellation).
	UpstreamStatus() int
}

// StatusOverloaded is Anthropic's non-standard "overloaded" status. net/http
// has no constant for it, and it is the one 5xx that means "come back later"
// rather than "this upstream broke", so it is named here instead of being a
// bare 529 in a switch.
const StatusOverloaded = 529

// usableStatus reports whether a status code carries enough information to
// classify by. Anything below 100 is a zero value or a bug, not a status.
func usableStatus(status int) bool { return status >= 100 }

// WithUpstreamStatus attaches the upstream's HTTP status to err, so Wrap
// classifies by it rather than by substrings of the body. This is the
// sanctioned way to satisfy UpstreamStatusHinter for a provider whose error is
// a plain fmt.Errorf; a provider whose own error type already carries the
// status should implement the interface on that type instead. Mirrors
// WithRetryAfter, and composes with it in either order.
//
// Returns err unchanged when it is nil or the status is unusable, so a caller
// need not branch on whether it has a status to give.
func WithUpstreamStatus(err error, status int) error {
	if err == nil || !usableStatus(status) {
		return err
	}
	return &upstreamStatusError{err: err, status: status}
}

type upstreamStatusError struct {
	err    error
	status int
}

func (e *upstreamStatusError) Error() string       { return e.err.Error() }
func (e *upstreamStatusError) Unwrap() error       { return e.err }
func (e *upstreamStatusError) UpstreamStatus() int { return e.status }
