package accounting

import "errors"

var (
	ErrLimitExceeded     = errors.New("accounting limit exceeded")
	ErrDuplicateRequest  = errors.New("accounting request already exists")
	ErrUnknownRequest    = errors.New("accounting request does not exist")
	ErrInvalidTransition = errors.New("invalid accounting state transition")
)
