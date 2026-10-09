package nats

import "errors"

var (
	ErrTimeout      = errors.New("nats: request timeout")
	ErrNoResponders = errors.New("nats: no responders")
	ErrClosed       = errors.New("nats: connection closed")
	ErrCancelled    = errors.New("nats: request cancelled")
)

type permanentError struct{ err error }

func (e permanentError) Error() string   { return e.err.Error() }
func (e permanentError) Unwrap() error   { return e.err }
func (e permanentError) Permanent() bool { return true }

// Permanent marks a consumer error as non-retryable. JetStream adapters may
// terminate that delivery immediately instead of exhausting MaxDeliver.
func Permanent(err error) error {
	if err == nil || IsPermanent(err) {
		return err
	}
	return permanentError{err: err}
}

// IsPermanent reports whether any error in the chain is explicitly terminal.
func IsPermanent(err error) bool {
	var marked interface{ Permanent() bool }
	return errors.As(err, &marked) && marked.Permanent()
}
