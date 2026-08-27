package offline

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	keyMigrationStagingFile       = "profile.key.next"
	keyMigrationSourceBackendFile = "key-migration.source-backend"
	keyMigrationPassphraseLocator = "profile.key"
)

func (d keyMigrationDetail) validate(state string) error {
	if d.MigrationVersion != 1 {
		return errors.New("unsupported key migration version")
	}
	if _, err := parseUUID(d.ProfileID); err != nil {
		return errors.New("invalid key migration profile")
	}
	source, err := parseKeyBackendName(d.SourceBackend)
	if err != nil {
		return err
	}
	destination, err := parseKeyBackendName(d.DestinationBackend)
	if err != nil || source == destination {
		return errors.New("invalid key migration backend transition")
	}
	if err := validateKeyLocator(source, d.SourceLocator); err != nil {
		return err
	}
	if err := validateKeyLocator(destination, d.DestinationLocator); err != nil {
		return err
	}
	identity, err := base64.RawURLEncoding.Strict().DecodeString(d.KeyIdentity)
	if err != nil || len(identity) != sha256.Size || base64.RawURLEncoding.EncodeToString(identity) != d.KeyIdentity {
		return errors.New("invalid key migration key identity")
	}
	if err := validateSHA256Hex(d.SourceKeyDigest); err != nil {
		return errors.New("invalid key migration source digest")
	}
	switch state {
	case keyMigrationStatePrepared:
		if d.DestinationKeyDigest != "" {
			return errors.New("prepared key migration has destination digest")
		}
	case keyMigrationStateDestinationVerified, keyMigrationStateAuthoritySwitched, keyMigrationStateSourceCleaned, keyMigrationStateCompleted:
		if err := validateSHA256Hex(d.DestinationKeyDigest); err != nil {
			return errors.New("invalid key migration destination digest")
		}
	default:
		return ErrInvalidState
	}
	return nil
}

func validateKeyLocator(backend uint8, locator string) error {
	if backend == KeyBackendPassphrase {
		if locator != keyMigrationPassphraseLocator {
			return errors.New("invalid passphrase key locator")
		}
		return nil
	}
	if len(locator) == 0 || len(locator) > 256 || strings.TrimSpace(locator) != locator {
		return errors.New("invalid system key locator")
	}
	for _, current := range locator {
		if current < 0x21 || current > 0x7e {
			return errors.New("invalid system key locator")
		}
	}
	return nil
}

func validateSHA256Hex(value string) error {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size || strings.ToLower(value) != value {
		return errors.New("invalid SHA-256 digest")
	}
	return nil
}

func keyBackendName(backend uint8) (string, error) {
	switch backend {
	case KeyBackendPassphrase:
		return "passphrase", nil
	case KeyBackendSystem:
		return "system", nil
	default:
		return "", ErrKeyUnavailable
	}
}

func parseKeyBackendName(value string) (uint8, error) {
	switch value {
	case "passphrase":
		return KeyBackendPassphrase, nil
	case "system":
		return KeyBackendSystem, nil
	default:
		return 0, errors.New("invalid key backend")
	}
}

func InspectKey(ctx context.Context, root string, expected Binding) (KeyMetadata, error) {
	if err := expected.validate(false); err != nil {
		return KeyMetadata{}, err
	}
	lease, err := AcquireLease(ctx, root, LeaseShared, DefaultLeaseTimeout)
	if err != nil {
		return KeyMetadata{}, err
	}
	defer lease.Close()
	keyFile, header, err := readKeyFileLocked(root)
	zeroBytes(keyFile)
	if err != nil {
		return KeyMetadata{}, err
	}
	if !header.Binding().matches(expected) {
		return KeyMetadata{}, ErrBindingMismatch
	}
	return KeyMetadata{Backend: header.Backend, Binding: header.Binding()}, nil
}

func readKeyFileLocked(root string) ([]byte, KeyHeader, error) {
	keyFile, err := readManaged(root, "profile.key", KeyHeaderSize+48)
	if err != nil {
		return nil, KeyHeader{}, ErrKeyUnavailable
	}
	if len(keyFile) != KeyHeaderSize+48 {
		zeroBytes(keyFile)
		return nil, KeyHeader{}, fmt.Errorf("%w: key file size", ErrCorruptStore)
	}
	header, err := DecodeKeyHeader(keyFile[:KeyHeaderSize])
	if err != nil {
		zeroBytes(keyFile)
		return nil, KeyHeader{}, err
	}
	return keyFile, header, nil
}

func openStoreLocked(root string, expected Binding, expectedTrust TrustState, wrappingMaterial []byte, expectedBackend uint8, allowKeyMigration bool) (*Store, KeyHeader, error) {
	keyFile, header, err := readKeyFileLocked(root)
	if err != nil {
		return nil, KeyHeader{}, err
	}
	defer zeroBytes(keyFile)
	if expectedBackend != 0 && header.Backend != expectedBackend {
		return nil, KeyHeader{}, ErrKeyAuthorityChanged
	}
	if !header.Binding().matches(expected) {
		return nil, KeyHeader{}, ErrBindingMismatch
	}
	_, dek, err := unwrapKey(keyFile, wrappingMaterial, expected)
	if err != nil {
		return nil, KeyHeader{}, err
	}
	binding := header.Binding()
	store := &Store{
		root: root, binding: binding,
		trustRoot: TrustState{canonical: expectedTrust.Bytes()},
		trust:     TrustState{canonical: expectedTrust.Bytes()},
		dek:       dek, leaseTimeout: DefaultLeaseTimeout,
	}
	failed := true
	defer func() {
		if failed {
			zeroBytes(dek)
		}
	}()
	if err := store.cleanTemporaryFilesLocked(); err != nil {
		return nil, KeyHeader{}, err
	}
	var profile ProfileRecord
	if err := store.readRecordLocked(ObjectProfile, binding.ProfileUUID(), &profile); err != nil {
		return nil, KeyHeader{}, err
	}
	if err := profile.validate(binding, expectedTrust); err != nil {
		return nil, KeyHeader{}, err
	}
	store.trustRoot = TrustState{canonical: profile.trustRoot()}
	store.trust = TrustState{canonical: append(json.RawMessage(nil), profile.TrustState...)}
	if _, err := store.scanNonceStateLocked(); err != nil {
		return nil, KeyHeader{}, err
	}
	if err := store.recoverLockedAllowKeyMigration(allowKeyMigration); err != nil {
		return nil, KeyHeader{}, err
	}
	if _, err := store.scanNonceStateLocked(); err != nil {
		return nil, KeyHeader{}, err
	}
	journal, _, journalErr := store.keyMigrationJournalLocked()
	stageExists, stageErr := migrationStageExists(root)
	if stageErr != nil {
		return nil, KeyHeader{}, stageErr
	}
	_, markerErr := migrationSourceBackend(root)
	markerExists := markerErr == nil
	if markerErr != nil && !errors.Is(markerErr, ErrNotFound) {
		return nil, KeyHeader{}, markerErr
	}
	if errors.Is(journalErr, ErrNotFound) && (stageExists || markerExists) {
		return nil, KeyHeader{}, fmt.Errorf("%w: orphaned key migration artifact", ErrCorruptStore)
	}
	if journalErr != nil && !errors.Is(journalErr, ErrNotFound) {
		return nil, KeyHeader{}, journalErr
	}
	if journal.JournalID != "" && !allowKeyMigration {
		return nil, KeyHeader{}, ErrKeyMigrationPending
	}
	failed = false
	return store, header, nil
}

type KeyMigrationSession struct {
	store          *Store
	lease          *Lease
	currentBackend uint8
	unlockMaterial []byte
	closed         bool
}

func BeginKeyMigration(ctx context.Context, root string, expected Binding, expectedTrust TrustState, wrappingMaterial []byte, expectedBackend uint8) (*KeyMigrationSession, error) {
	if err := expected.validate(false); err != nil {
		return nil, err
	}
	if !expectedTrust.valid() || len(wrappingMaterial) == 0 {
		return nil, ErrKeyUnavailable
	}
	if expectedBackend != KeyBackendPassphrase && expectedBackend != KeyBackendSystem {
		return nil, ErrKeyUnavailable
	}
	lease, err := AcquireLease(ctx, root, LeaseExclusive, DefaultLeaseTimeout)
	if err != nil {
		return nil, err
	}
	store, header, err := openStoreLocked(root, expected, expectedTrust, wrappingMaterial, expectedBackend, true)
	if err != nil {
		_ = lease.Close()
		return nil, err
	}
	return &KeyMigrationSession{
		store: store, lease: lease, currentBackend: header.Backend,
		unlockMaterial: append([]byte(nil), wrappingMaterial...),
	}, nil
}

func (s *KeyMigrationSession) Close() error {
	if s == nil || s.closed {
		return nil
	}
	s.closed = true
	zeroBytes(s.unlockMaterial)
	s.unlockMaterial = nil
	storeErr := s.store.Close()
	leaseErr := s.lease.Close()
	if storeErr != nil {
		return storeErr
	}
	return leaseErr
}

func (s *KeyMigrationSession) Migrate(options KeyMigrationOptions) (KeyMigrationResult, error) {
	if s == nil || s.closed || s.store == nil || s.store.closed {
		return KeyMigrationResult{}, ErrClosed
	}
	if options.DestinationBackend != KeyBackendPassphrase && options.DestinationBackend != KeyBackendSystem {
		return KeyMigrationResult{}, ErrKeyMigrationConflict
	}
	if (s.currentBackend == KeyBackendSystem || options.DestinationBackend == KeyBackendSystem) && validateKeyLocator(KeyBackendSystem, options.SystemLocator) != nil {
		return KeyMigrationResult{}, ErrKeyMigrationConflict
	}

	journal, detail, err := s.store.keyMigrationJournalLocked()
	resumed := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return KeyMigrationResult{}, err
	}
	if errors.Is(err, ErrNotFound) {
		if s.currentBackend == options.DestinationBackend {
			return KeyMigrationResult{SourceBackend: s.currentBackend, DestinationBackend: options.DestinationBackend}, nil
		}
		if exists, stageErr := migrationStageExists(s.store.root); stageErr != nil {
			return KeyMigrationResult{}, stageErr
		} else if exists {
			return KeyMigrationResult{}, fmt.Errorf("%w: orphaned key migration staging file", ErrCorruptStore)
		}
		journal, detail, err = s.startKeyMigrationLocked(options)
		if err != nil {
			return KeyMigrationResult{}, err
		}
		if err := runKeyMigrationFailpoint(options, KeyMigrationAfterJournalDurable); err != nil {
			return KeyMigrationResult{}, err
		}
	} else {
		destination, parseErr := parseKeyBackendName(detail.DestinationBackend)
		if parseErr != nil || destination != options.DestinationBackend {
			return KeyMigrationResult{}, ErrKeyMigrationConflict
		}
		if detail.KeyIdentity != keyIdentity(s.store.binding, s.store.dek) {
			return KeyMigrationResult{}, ErrKeyMigrationMismatch
		}
	}
	if err := s.ensureMigrationSourceBackendLocked(detail); err != nil {
		return KeyMigrationResult{}, err
	}

	sourceBackend, _ := parseKeyBackendName(detail.SourceBackend)
	destinationBackend, _ := parseKeyBackendName(detail.DestinationBackend)
	result := KeyMigrationResult{SourceBackend: sourceBackend, DestinationBackend: destinationBackend, Changed: true, Resumed: resumed}
	for {
		switch journal.State {
		case keyMigrationStatePrepared:
			destinationKey, digest, prepareErr := s.prepareDestinationLocked(detail, options)
			if prepareErr != nil {
				return KeyMigrationResult{}, prepareErr
			}
			zeroBytes(destinationKey)
			detail.DestinationKeyDigest = digest
			if err := s.advanceKeyMigrationLocked(&journal, detail, keyMigrationStateDestinationVerified); err != nil {
				return KeyMigrationResult{}, err
			}
			if err := runKeyMigrationFailpoint(options, KeyMigrationAfterDestinationVerify); err != nil {
				return KeyMigrationResult{}, err
			}
		case keyMigrationStateDestinationVerified:
			if err := s.switchKeyAuthorityLocked(detail, options); err != nil {
				return KeyMigrationResult{}, err
			}
			if err := s.advanceKeyMigrationLocked(&journal, detail, keyMigrationStateAuthoritySwitched); err != nil {
				return KeyMigrationResult{}, err
			}
		case keyMigrationStateAuthoritySwitched:
			if err := runKeyMigrationFailpoint(options, KeyMigrationBeforeSourceCleanup); err != nil {
				return KeyMigrationResult{}, err
			}
			if err := s.cleanupSourceLocked(detail, options); err != nil {
				return KeyMigrationResult{}, err
			}
			if err := runKeyMigrationFailpoint(options, KeyMigrationAfterSourceCleanup); err != nil {
				return KeyMigrationResult{}, err
			}
			if err := s.advanceKeyMigrationLocked(&journal, detail, keyMigrationStateSourceCleaned); err != nil {
				return KeyMigrationResult{}, err
			}
		case keyMigrationStateSourceCleaned:
			if err := s.verifyMigrationCompletionLocked(detail, options); err != nil {
				return KeyMigrationResult{}, err
			}
			if err := deleteManaged(s.store.root, keyMigrationStagingFile); err != nil {
				return KeyMigrationResult{}, err
			}
			if err := deleteManaged(s.store.root, keyMigrationSourceBackendFile); err != nil {
				return KeyMigrationResult{}, err
			}
			if err := s.advanceKeyMigrationLocked(&journal, detail, keyMigrationStateCompleted); err != nil {
				return KeyMigrationResult{}, err
			}
			if err := runKeyMigrationFailpoint(options, KeyMigrationAfterJournalCompletion); err != nil {
				return KeyMigrationResult{}, err
			}
		case keyMigrationStateCompleted:
			if err := s.verifyMigrationCompletionLocked(detail, options); err != nil {
				return KeyMigrationResult{}, err
			}
			if err := deleteManaged(s.store.root, keyMigrationStagingFile); err != nil {
				return KeyMigrationResult{}, err
			}
			if err := deleteManaged(s.store.root, keyMigrationSourceBackendFile); err != nil {
				return KeyMigrationResult{}, err
			}
			if err := deleteManaged(s.store.root, objectRelative(ObjectJournal, journal.JournalID)); err != nil {
				return KeyMigrationResult{}, err
			}
			s.currentBackend = destinationBackend
			return result, nil
		default:
			return KeyMigrationResult{}, ErrInvalidState
		}
	}
}

func (s *KeyMigrationSession) startKeyMigrationLocked(options KeyMigrationOptions) (JournalRecord, keyMigrationDetail, error) {
	keyFile, header, err := readKeyFileLocked(s.store.root)
	if err != nil {
		return JournalRecord{}, keyMigrationDetail{}, err
	}
	defer zeroBytes(keyFile)
	if header.Backend != s.currentBackend {
		return JournalRecord{}, keyMigrationDetail{}, ErrKeyAuthorityChanged
	}
	sourceName, _ := keyBackendName(s.currentBackend)
	destinationName, _ := keyBackendName(options.DestinationBackend)
	sourceLocator := keyMigrationPassphraseLocator
	if s.currentBackend == KeyBackendSystem {
		sourceLocator = options.SystemLocator
	}
	destinationLocator := keyMigrationPassphraseLocator
	if options.DestinationBackend == KeyBackendSystem {
		destinationLocator = options.SystemLocator
	}
	detail := keyMigrationDetail{
		MigrationVersion: 1, ProfileID: s.store.binding.ProfileUUID(),
		SourceBackend: sourceName, DestinationBackend: destinationName,
		SourceLocator: sourceLocator, DestinationLocator: destinationLocator,
		KeyIdentity:     keyIdentity(s.store.binding, s.store.dek),
		SourceKeyDigest: keyFileDigest(keyFile), DestinationKeyDigest: "",
	}
	detailBytes, err := marshalCanonical(detail)
	if err != nil {
		return JournalRecord{}, keyMigrationDetail{}, err
	}
	journalID, err := randomUUID()
	if err != nil {
		return JournalRecord{}, keyMigrationDetail{}, err
	}
	journal := newJournal(formatUUID(journalID), JournalKeyMigration, keyMigrationStatePrepared, detail.ProfileID, detail.ProfileID, 1, detailBytes)
	if err := s.store.writeRecordLocked(ObjectJournal, journal.JournalID, journal, false); err != nil {
		return JournalRecord{}, keyMigrationDetail{}, err
	}
	return journal, detail, nil
}

func (s *KeyMigrationSession) prepareDestinationLocked(detail keyMigrationDetail, options KeyMigrationOptions) ([]byte, string, error) {
	stage, stageErr := readManaged(s.store.root, keyMigrationStagingFile, KeyHeaderSize+48)
	stageExists := stageErr == nil
	if stageErr != nil && !errors.Is(stageErr, ErrNotFound) {
		return nil, "", stageErr
	}
	if stageExists && len(stage) != KeyHeaderSize+48 {
		zeroBytes(stage)
		return nil, "", fmt.Errorf("%w: key migration staging size", ErrCorruptStore)
	}
	material, err := s.destinationMaterialLocked(detail, options, stageExists)
	if err != nil {
		zeroBytes(stage)
		return nil, "", err
	}
	defer zeroBytes(material)
	if !stageExists {
		destinationBackend, _ := parseKeyBackendName(detail.DestinationBackend)
		stage, err = wrapExistingKeyForBackend(s.store.binding, material, s.store.dek, destinationBackend)
		if err != nil {
			return nil, "", err
		}
		if err := atomicWriteManaged(s.store.root, keyMigrationStagingFile, stage, false); err != nil {
			zeroBytes(stage)
			return nil, "", fmt.Errorf("write key migration destination: %w", err)
		}
		if err := runKeyMigrationFailpoint(options, KeyMigrationAfterDestinationWrite); err != nil {
			zeroBytes(stage)
			return nil, "", err
		}
		zeroBytes(stage)
		stage, err = readManaged(s.store.root, keyMigrationStagingFile, KeyHeaderSize+48)
		if err != nil {
			return nil, "", err
		}
	}
	if err := s.verifyDestinationKeyFile(stage, material, detail); err != nil {
		zeroBytes(stage)
		return nil, "", err
	}
	return stage, keyFileDigest(stage), nil
}

func (s *KeyMigrationSession) destinationMaterialLocked(detail keyMigrationDetail, options KeyMigrationOptions, stageExists bool) ([]byte, error) {
	destinationBackend, _ := parseKeyBackendName(detail.DestinationBackend)
	if destinationBackend == KeyBackendPassphrase {
		material := options.DestinationPassphrase
		if len(material) == 0 && s.currentBackend == KeyBackendPassphrase {
			material = s.unlockMaterial
		}
		if len(material) == 0 {
			return nil, ErrKeyUnavailable
		}
		return append([]byte(nil), material...), nil
	}
	if options.SystemKeys == nil {
		return nil, ErrKeyBackendUnavailable
	}
	secret, err := options.SystemKeys.Load(detail.DestinationLocator)
	if err == nil {
		if len(secret) != 32 {
			zeroBytes(secret)
			return nil, ErrKeyBackendUnavailable
		}
		return secret, nil
	}
	if !errors.Is(err, ErrSystemKeyNotFound) {
		return nil, ErrKeyBackendUnavailable
	}
	if stageExists {
		return nil, ErrKeyMigrationMismatch
	}
	generated, err := options.SystemKeys.Generate()
	if err != nil || len(generated) != 32 {
		zeroBytes(generated)
		return nil, ErrKeyBackendUnavailable
	}
	defer zeroBytes(generated)
	if err := options.SystemKeys.Store(detail.DestinationLocator, generated); err != nil {
		return nil, ErrKeyBackendUnavailable
	}
	if err := runKeyMigrationFailpoint(options, KeyMigrationAfterDestinationStore); err != nil {
		return nil, err
	}
	loaded, err := options.SystemKeys.Load(detail.DestinationLocator)
	if err != nil || !bytes.Equal(loaded, generated) {
		zeroBytes(loaded)
		return nil, ErrKeyMigrationMismatch
	}
	return loaded, nil
}

func (s *KeyMigrationSession) verifyDestinationKeyFile(keyFile, material []byte, detail keyMigrationDetail) error {
	destinationBackend, _ := parseKeyBackendName(detail.DestinationBackend)
	if len(keyFile) != KeyHeaderSize+48 {
		return ErrKeyMigrationMismatch
	}
	header, dek, err := unwrapKey(keyFile, material, s.store.binding)
	if err != nil {
		return ErrKeyMigrationMismatch
	}
	defer zeroBytes(dek)
	if header.Backend != destinationBackend || !bytes.Equal(dek, s.store.dek) || keyIdentity(s.store.binding, dek) != detail.KeyIdentity {
		return ErrKeyMigrationMismatch
	}
	return nil
}

func (s *KeyMigrationSession) switchKeyAuthorityLocked(detail keyMigrationDetail, options KeyMigrationOptions) error {
	current, header, err := readKeyFileLocked(s.store.root)
	if err != nil {
		return err
	}
	defer zeroBytes(current)
	currentDigest := keyFileDigest(current)
	destinationBackend, _ := parseKeyBackendName(detail.DestinationBackend)
	if currentDigest == detail.DestinationKeyDigest {
		if header.Backend != destinationBackend {
			return ErrKeyMigrationMismatch
		}
		s.currentBackend = destinationBackend
		return nil
	}
	if currentDigest != detail.SourceKeyDigest {
		return ErrKeyMigrationMismatch
	}
	stage, err := readManaged(s.store.root, keyMigrationStagingFile, KeyHeaderSize+48)
	if err != nil {
		return ErrKeyMigrationMismatch
	}
	defer zeroBytes(stage)
	if keyFileDigest(stage) != detail.DestinationKeyDigest {
		return ErrKeyMigrationMismatch
	}
	material, err := s.destinationMaterialLocked(detail, options, true)
	if err != nil {
		return err
	}
	defer zeroBytes(material)
	if err := s.verifyDestinationKeyFile(stage, material, detail); err != nil {
		return err
	}
	if err := atomicWriteManaged(s.store.root, "profile.key", stage, true); err != nil {
		return fmt.Errorf("switch offline key authority: %w", err)
	}
	s.currentBackend = destinationBackend
	if err := runKeyMigrationFailpoint(options, KeyMigrationAfterAuthoritySwitch); err != nil {
		return err
	}
	published, publishedHeader, err := readKeyFileLocked(s.store.root)
	if err != nil {
		return err
	}
	defer zeroBytes(published)
	if keyFileDigest(published) != detail.DestinationKeyDigest || publishedHeader.Backend != destinationBackend {
		return ErrKeyMigrationMismatch
	}
	return nil
}

func (s *KeyMigrationSession) cleanupSourceLocked(detail keyMigrationDetail, options KeyMigrationOptions) error {
	sourceBackend, _ := parseKeyBackendName(detail.SourceBackend)
	if sourceBackend == KeyBackendPassphrase {
		current, _, err := readKeyFileLocked(s.store.root)
		if err != nil {
			return err
		}
		defer zeroBytes(current)
		if keyFileDigest(current) != detail.DestinationKeyDigest {
			return ErrKeyMigrationMismatch
		}
		return nil
	}
	if options.SystemKeys == nil {
		return ErrKeyBackendUnavailable
	}
	if err := options.SystemKeys.Delete(detail.SourceLocator); err != nil && !errors.Is(err, ErrSystemKeyNotFound) {
		return ErrKeyBackendUnavailable
	}
	remaining, err := options.SystemKeys.Load(detail.SourceLocator)
	zeroBytes(remaining)
	if err == nil {
		return ErrKeyMigrationMismatch
	}
	if !errors.Is(err, ErrSystemKeyNotFound) {
		return ErrKeyBackendUnavailable
	}
	return nil
}

func (s *KeyMigrationSession) verifyMigrationCompletionLocked(detail keyMigrationDetail, options KeyMigrationOptions) error {
	current, header, err := readKeyFileLocked(s.store.root)
	if err != nil {
		return err
	}
	defer zeroBytes(current)
	destinationBackend, _ := parseKeyBackendName(detail.DestinationBackend)
	if header.Backend != destinationBackend || keyFileDigest(current) != detail.DestinationKeyDigest {
		return ErrKeyMigrationMismatch
	}
	sourceBackend, _ := parseKeyBackendName(detail.SourceBackend)
	if sourceBackend != KeyBackendSystem {
		return nil
	}
	if options.SystemKeys == nil {
		return ErrKeyBackendUnavailable
	}
	remaining, loadErr := options.SystemKeys.Load(detail.SourceLocator)
	zeroBytes(remaining)
	if loadErr == nil {
		return ErrKeyMigrationMismatch
	}
	if !errors.Is(loadErr, ErrSystemKeyNotFound) {
		return ErrKeyBackendUnavailable
	}
	return nil
}

func (s *Store) keyMigrationJournalLocked() (JournalRecord, keyMigrationDetail, error) {
	journals, err := s.journalRecordsLocked()
	if err != nil {
		return JournalRecord{}, keyMigrationDetail{}, err
	}
	var found JournalRecord
	var detail keyMigrationDetail
	for _, journal := range journals {
		if journal.Kind != JournalKeyMigration {
			continue
		}
		if found.JournalID != "" {
			return JournalRecord{}, keyMigrationDetail{}, fmt.Errorf("%w: multiple key migration journals", ErrCorruptStore)
		}
		if err := decodeClosed(journal.Detail, &detail); err != nil || detail.validate(journal.State) != nil {
			return JournalRecord{}, keyMigrationDetail{}, fmt.Errorf("%w: invalid key migration journal", ErrCorruptStore)
		}
		found = journal
	}
	if found.JournalID == "" {
		return JournalRecord{}, keyMigrationDetail{}, ErrNotFound
	}
	return found, detail, nil
}

func (s *KeyMigrationSession) advanceKeyMigrationLocked(journal *JournalRecord, detail keyMigrationDetail, next string) error {
	allowed := map[string]string{
		keyMigrationStatePrepared:            keyMigrationStateDestinationVerified,
		keyMigrationStateDestinationVerified: keyMigrationStateAuthoritySwitched,
		keyMigrationStateAuthoritySwitched:   keyMigrationStateSourceCleaned,
		keyMigrationStateSourceCleaned:       keyMigrationStateCompleted,
	}
	if allowed[journal.State] != next {
		return ErrInvalidState
	}
	revision, err := parseCanonicalUint(journal.Revision, true)
	if err != nil || revision >= 5 {
		return ErrInvalidState
	}
	detailBytes, err := marshalCanonical(detail)
	if err != nil {
		return err
	}
	journal.State = next
	journal.Revision = canonicalUint(revision + 1)
	journal.Detail = detailBytes
	if err := journal.validate(); err != nil {
		return err
	}
	return s.store.writeRecordLocked(ObjectJournal, journal.JournalID, *journal, true)
}

func (s *KeyMigrationSession) ensureMigrationSourceBackendLocked(detail keyMigrationDetail) error {
	sourceBackend, err := parseKeyBackendName(detail.SourceBackend)
	if err != nil {
		return ErrKeyMigrationConflict
	}
	persisted, readErr := migrationSourceBackend(s.store.root)
	if readErr == nil {
		if persisted != sourceBackend {
			return ErrKeyMigrationMismatch
		}
		return nil
	}
	if !errors.Is(readErr, ErrNotFound) {
		return readErr
	}
	return atomicWriteManaged(s.store.root, keyMigrationSourceBackendFile, []byte{sourceBackend}, false)
}

func migrationSourceBackend(root string) (uint8, error) {
	marker, err := readManaged(root, keyMigrationSourceBackendFile, 1)
	if err != nil {
		return 0, err
	}
	if len(marker) != 1 || (marker[0] != KeyBackendPassphrase && marker[0] != KeyBackendSystem) {
		return 0, fmt.Errorf("%w: invalid key migration source marker", ErrCorruptStore)
	}
	return marker[0], nil
}

func migrationStageExists(root string) (bool, error) {
	stage, err := readManaged(root, keyMigrationStagingFile, KeyHeaderSize+48)
	zeroBytes(stage)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func runKeyMigrationFailpoint(options KeyMigrationOptions, point KeyMigrationFailpoint) error {
	if options.Failpoint == nil {
		return nil
	}
	return options.Failpoint(point)
}

func keyFileDigest(keyFile []byte) string {
	digest := sha256.Sum256(keyFile)
	return hex.EncodeToString(digest[:])
}

func keyIdentity(binding Binding, dek []byte) string {
	mac := hmac.New(sha256.New, dek)
	_, _ = mac.Write([]byte("edu-agent-offline-key-identity-v1\x00"))
	_, _ = mac.Write(binding.OriginHash[:])
	_, _ = mac.Write(binding.DeviceID[:])
	var generation [8]byte
	binary.BigEndian.PutUint64(generation[:], binding.LearnerGeneration)
	_, _ = mac.Write(generation[:])
	_, _ = mac.Write(binding.ProfileID[:])
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
