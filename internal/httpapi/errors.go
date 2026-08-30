package httpapi

import "errors"

type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }

func newValidationError(msg string) error {
	return &validationError{msg: msg}
}

func isValidationError(err error) bool {
	var ve *validationError
	return errors.As(err, &ve)
}
