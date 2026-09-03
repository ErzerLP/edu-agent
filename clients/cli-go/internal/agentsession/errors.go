package agentsession

import "errors"

const (
	ErrorCodeCorrupt            = "session_corrupt"
	ErrorCodeVersionUnsupported = "session_version_unsupported"
)

var (
	ErrKeyUnavailable       = errors.New("agent session native key service unavailable")
	ErrNotFound             = errors.New("agent session not found")
	ErrInUse                = errors.New("agent session is already open for writing")
	ErrCorrupt              = errors.New("agent session data is corrupt")
	ErrVersionUnsupported   = errors.New("agent session version is unsupported")
	ErrWrongProfile         = errors.New("agent session belongs to a different profile")
	ErrCheckpointConflict   = errors.New("agent session checkpoint revision conflict")
	ErrStoreFull            = errors.New("agent session store quota exceeded")
	ErrCheckpointSaveFailed = errors.New("agent session checkpoint was not published")
	ErrDeleteFailed         = errors.New("agent session deletion or clear cleanup failed")
	ErrOutcomeUnknown       = errors.New("agent session persistence outcome is unknown")
	ErrPrivacyInvalidated   = errors.New("agent session was invalidated by privacy clear")
	ErrInvalid              = errors.New("agent session data is invalid")
)

// StableErrorCode exposes only stable, presentation-safe compatibility
// classifications. Callers must not surface the wrapped raw decode error.
func StableErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrVersionUnsupported):
		return ErrorCodeVersionUnsupported
	case errors.Is(err, ErrCorrupt):
		return ErrorCodeCorrupt
	default:
		return ""
	}
}
