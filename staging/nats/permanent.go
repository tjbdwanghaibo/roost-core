package nats

import "errors"

type permanentError struct{ err error }

func (e permanentError) Error() string   { return e.err.Error() }
func (e permanentError) Unwrap() error   { return e.err }
func (e permanentError) Permanent() bool { return true }

// Permanent is the rolling-upgrade-compatible form of core/nats.Permanent.
// Both expose the same structural marker understood by this adapter.
func Permanent(err error) error {
	if err == nil || isPermanent(err) {
		return err
	}
	return permanentError{err: err}
}

func isPermanent(err error) bool {
	var marked interface{ Permanent() bool }
	return errors.As(err, &marked) && marked.Permanent()
}
