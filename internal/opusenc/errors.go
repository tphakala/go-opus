package opusenc

import "errors"

// Sentinel errors returned by the exported opusenc surface, mapping from the
// libopus OPUS_* codes the internal (int-returning) transliteration uses. The
// public opus package re-wraps these to its own sentinels. Mirrors
// internal/opusdec/errors.go.
var (
	// ErrBadArg maps OPUS_BAD_ARG.
	ErrBadArg = errors.New("opusenc: bad argument")
	// ErrBufferTooSmall maps OPUS_BUFFER_TOO_SMALL.
	ErrBufferTooSmall = errors.New("opusenc: buffer too small")
	// ErrInternal maps OPUS_INTERNAL_ERROR.
	ErrInternal = errors.New("opusenc: internal error")
)

// codeErr maps an internal negative Opus error code to its sentinel error. A
// non-negative code (including opusOK) is not an error and maps to ErrInternal
// defensively, since codeErr is only called on the error path.
func codeErr(code int) error {
	switch code {
	case opusBadArg:
		return ErrBadArg
	case opusBufferTooSmall:
		return ErrBufferTooSmall
	default: // opusInternalError and any unexpected code
		return ErrInternal
	}
}
