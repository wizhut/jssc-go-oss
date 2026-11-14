package jssc

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by this package. Every error it produces wraps one
// of these, so callers can classify failures with errors.Is without matching
// on message text.
var (
	// ErrBadMagic means the input does not begin with a known JSSC magic.
	ErrBadMagic = errors.New("jssc: not a jssc archive")

	// ErrTruncated means the archive ended before a declared field did.
	ErrTruncated = errors.New("jssc: truncated archive")

	// ErrCorrupt means the archive is internally inconsistent: an out-of-range
	// index, an unknown tag, a length that does not add up.
	ErrCorrupt = errors.New("jssc: corrupt archive")

	// ErrUnsupported means the archive, or the requested encoding, uses a
	// feature this build does not implement — an unregistered pool codec, an
	// unknown structure codec, an index width outside {1,2,4}.
	ErrUnsupported = errors.New("jssc: unsupported")

	// ErrInvalidInput means the input to an encoder was not what it claimed:
	// empty, malformed JSON, or a JSON value the format cannot represent.
	ErrInvalidInput = errors.New("jssc: invalid input")

	// ErrLimitExceeded means a declared or derived size exceeded a format limit
	// (specification §14) or a caller-supplied decode limit (§14).
	ErrLimitExceeded = errors.New("jssc: limit exceeded")
)

func errTruncatedf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrTruncated, fmt.Sprintf(format, args...))
}

func errCorruptf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrupt, fmt.Sprintf(format, args...))
}

func errUnsupportedf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsupported, fmt.Sprintf(format, args...))
}

func errInvalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, fmt.Sprintf(format, args...))
}

func errLimitf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrLimitExceeded, fmt.Sprintf(format, args...))
}
