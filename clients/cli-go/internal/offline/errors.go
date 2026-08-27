package offline

import "errors"

var (
	ErrNotFound              = errors.New("offline_not_found")
	ErrProfileExists         = errors.New("offline_profile_exists")
	ErrProfileBusy           = errors.New("offline_profile_busy")
	ErrKeyUnavailable        = errors.New("offline_key_unavailable")
	ErrKeyBackendUnavailable = errors.New("offline_key_backend_unavailable")
	ErrKeyAuthorityChanged   = errors.New("offline_key_authority_changed")
	ErrKeyMigrationPending   = errors.New("offline_key_migration_pending")
	ErrKeyMigrationConflict  = errors.New("offline_key_migration_conflict")
	ErrKeyMigrationMismatch  = errors.New("offline_key_migration_mismatch")
	ErrSystemKeyNotFound     = errors.New("offline_system_key_not_found")
	ErrBindingMismatch       = errors.New("offline_binding_mismatch")
	ErrCorruptStore          = errors.New("offline_store_corrupt")
	ErrImmutableOperation    = errors.New("offline_operation_immutable")
	ErrCounterRollback       = errors.New("offline_nonce_counter_rollback")
	ErrCounterOverflow       = errors.New("offline_nonce_counter_overflow")
	ErrClosed                = errors.New("offline_store_closed")
	ErrUnsafePath            = errors.New("offline_unsafe_path")
	ErrInvalidState          = errors.New("offline_invalid_state")
)
