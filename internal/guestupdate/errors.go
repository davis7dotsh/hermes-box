package guestupdate

import "errors"

var (
	errBusy         = errors.New("busy")
	errInvalidInput = errors.New("invalid input")
	errIntegrity    = errors.New("integrity failure")
	errHealth       = errors.New("health failure")
)

// classifiedError keeps the operator-facing error unchanged while attaching a
// machine-readable class for the protocol boundary. Unwrap exposes both the
// class and the original cause so errors.Is and errors.As continue to work.
type classifiedError struct {
	class error
	err   error
}

func (err classifiedError) Error() string {
	return err.err.Error()
}

func (err classifiedError) Unwrap() []error {
	return []error{err.class, err.err}
}

func withClass(class, err error) error {
	if err == nil || errors.Is(err, class) {
		return err
	}
	return classifiedError{class: class, err: err}
}
