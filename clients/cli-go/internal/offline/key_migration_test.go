package offline

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testSystemLocator = "profile-test-system-locator"

var errMigrationCrash = errors.New("simulated key migration crash")

type fakeSystemKeyProvider struct {
	secrets     map[string][]byte
	generateErr error
	storeErr    error
	deleteErr   error
	loadErr     error
	nextSecret  byte
}

func newFakeSystemKeyProvider() *fakeSystemKeyProvider {
	return &fakeSystemKeyProvider{secrets: make(map[string][]byte), nextSecret: 0x41}
}

func (f *fakeSystemKeyProvider) Generate() ([]byte, error) {
	if f.generateErr != nil {
		return nil, f.generateErr
	}
	secret := bytes.Repeat([]byte{f.nextSecret}, 32)
	f.nextSecret++
	return secret, nil
}

func (f *fakeSystemKeyProvider) Load(locator string) ([]byte, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	secret, ok := f.secrets[locator]
	if !ok {
		return nil, ErrSystemKeyNotFound
	}
	return append([]byte(nil), secret...), nil
}

func (f *fakeSystemKeyProvider) Store(locator string, secret []byte) error {
	if f.storeErr != nil {
		return f.storeErr
	}
	f.secrets[locator] = append([]byte(nil), secret...)
	return nil
}

func (f *fakeSystemKeyProvider) Delete(locator string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.secrets, locator)
	return nil
}

func TestKeyMigrationCrashRecoveryAtEveryDurableBoundary(t *testing.T) {
	points := []KeyMigrationFailpoint{
		KeyMigrationAfterJournalDurable,
		KeyMigrationAfterDestinationStore,
		KeyMigrationAfterDestinationWrite,
		KeyMigrationAfterDestinationVerify,
		KeyMigrationAfterAuthoritySwitch,
		KeyMigrationBeforeSourceCleanup,
		KeyMigrationAfterSourceCleanup,
		KeyMigrationAfterJournalCompletion,
	}
	for _, point := range points {
		t.Run(string(point), func(t *testing.T) {
			root, binding, trust, store := createTestStore(t)
			pack := testPack(testPackID, "MIGRATION_CRASH_SECRET")
			if err := store.SavePack(t.Context(), pack); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			provider := newFakeSystemKeyProvider()
			session, err := BeginKeyMigration(t.Context(), root, binding, trust, testPassphrase, KeyBackendPassphrase)
			if err != nil {
				t.Fatal(err)
			}
			triggered := false
			_, err = session.Migrate(KeyMigrationOptions{
				DestinationBackend: KeyBackendSystem,
				SystemLocator:      testSystemLocator,
				SystemKeys:         provider,
				Failpoint: func(current KeyMigrationFailpoint) error {
					if !triggered && current == point {
						triggered = true
						return errMigrationCrash
					}
					return nil
				},
			})
			if !errors.Is(err, errMigrationCrash) || !triggered {
				t.Fatalf("migration error=%v triggered=%t", err, triggered)
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}

			metadata, err := InspectKey(t.Context(), root, binding)
			if err != nil {
				t.Fatal(err)
			}
			material := testPassphrase
			if metadata.Backend == KeyBackendSystem {
				material, err = provider.Load(testSystemLocator)
				if err != nil {
					t.Fatal(err)
				}
			}
			if opened, openErr := OpenWithBackend(t.Context(), root, binding, trust, material, metadata.Backend); !errors.Is(openErr, ErrKeyMigrationPending) {
				if opened != nil {
					_ = opened.Close()
				}
				t.Fatalf("normal open during migration=%v", openErr)
			}
			resume, err := BeginKeyMigration(t.Context(), root, binding, trust, material, metadata.Backend)
			zeroBytesIfOwned(metadata.Backend, material)
			if err != nil {
				t.Fatal(err)
			}
			result, err := resume.Migrate(KeyMigrationOptions{DestinationBackend: KeyBackendSystem, SystemLocator: testSystemLocator, SystemKeys: provider})
			if err != nil || !result.Changed || !result.Resumed {
				t.Fatalf("resume result=%+v err=%v", result, err)
			}
			if err := resume.Close(); err != nil {
				t.Fatal(err)
			}

			metadata, err = InspectKey(t.Context(), root, binding)
			if err != nil || metadata.Backend != KeyBackendSystem {
				t.Fatalf("final metadata=%+v err=%v", metadata, err)
			}
			secret, err := provider.Load(testSystemLocator)
			if err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenWithBackend(t.Context(), root, binding, trust, secret, KeyBackendSystem)
			zeroBytes(secret)
			if err != nil {
				t.Fatal(err)
			}
			got, err := reopened.GetPack(t.Context(), pack.ID)
			if err != nil || !bytes.Equal(got.Canonical, pack.Canonical) {
				t.Fatalf("reopened pack=%#v err=%v", got, err)
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(root, keyMigrationStagingFile)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("staging remains: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(root, keyMigrationSourceBackendFile)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("source marker remains: %v", err)
			}
			assertNoKeyMigrationJournal(t, root)
		})
	}
}

func TestKeyMigrationJournalIsClosedAndContainsNoRawKeyMaterial(t *testing.T) {
	root, binding, trust, store := createTestStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	provider := newFakeSystemKeyProvider()
	session, err := BeginKeyMigration(t.Context(), root, binding, trust, testPassphrase, KeyBackendPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	_, err = session.Migrate(KeyMigrationOptions{
		DestinationBackend: KeyBackendSystem, SystemLocator: testSystemLocator, SystemKeys: provider,
		Failpoint: func(point KeyMigrationFailpoint) error {
			if point == KeyMigrationAfterJournalDurable {
				return errMigrationCrash
			}
			return nil
		},
	})
	if !errors.Is(err, errMigrationCrash) {
		t.Fatal(err)
	}
	journal, detail, err := session.store.keyMigrationJournalLocked()
	if err != nil {
		t.Fatal(err)
	}
	if journal.State != keyMigrationStatePrepared || detail.SourceBackend != "passphrase" || detail.DestinationBackend != "system" || detail.ProfileID != session.store.binding.ProfileUUID() || detail.KeyIdentity == "" || detail.DestinationKeyDigest != "" {
		t.Fatalf("journal=%+v detail=%+v", journal, detail)
	}
	if bytes.Contains(journal.Detail, testPassphrase) {
		t.Fatal("journal detail contains the passphrase")
	}
	if err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return walkErr
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if bytes.Contains(data, testPassphrase) {
			t.Fatalf("raw passphrase found in %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestKeyMigrationDestinationFailuresNeverSwitchAuthority(t *testing.T) {
	t.Run("store failure", func(t *testing.T) {
		root, binding, trust, store := createTestStore(t)
		_ = store.Close()
		provider := newFakeSystemKeyProvider()
		provider.storeErr = errors.New("store failed")
		session, err := BeginKeyMigration(t.Context(), root, binding, trust, testPassphrase, KeyBackendPassphrase)
		if err != nil {
			t.Fatal(err)
		}
		_, err = session.Migrate(KeyMigrationOptions{DestinationBackend: KeyBackendSystem, SystemLocator: testSystemLocator, SystemKeys: provider})
		if !errors.Is(err, ErrKeyBackendUnavailable) {
			t.Fatalf("migration error=%v", err)
		}
		_ = session.Close()
		metadata, err := InspectKey(t.Context(), root, binding)
		if err != nil || metadata.Backend != KeyBackendPassphrase {
			t.Fatalf("authority switched after store failure: %+v %v", metadata, err)
		}
	})

	t.Run("destination mismatch", func(t *testing.T) {
		root, binding, trust, store := createTestStore(t)
		_ = store.Close()
		provider := newFakeSystemKeyProvider()
		session, err := BeginKeyMigration(t.Context(), root, binding, trust, testPassphrase, KeyBackendPassphrase)
		if err != nil {
			t.Fatal(err)
		}
		_, err = session.Migrate(KeyMigrationOptions{
			DestinationBackend: KeyBackendSystem, SystemLocator: testSystemLocator, SystemKeys: provider,
			Failpoint: func(point KeyMigrationFailpoint) error {
				if point == KeyMigrationAfterDestinationWrite {
					return errMigrationCrash
				}
				return nil
			},
		})
		if !errors.Is(err, errMigrationCrash) {
			t.Fatal(err)
		}
		_ = session.Close()
		provider.secrets[testSystemLocator] = bytes.Repeat([]byte{0xff}, 32)
		resume, err := BeginKeyMigration(t.Context(), root, binding, trust, testPassphrase, KeyBackendPassphrase)
		if err != nil {
			t.Fatal(err)
		}
		_, err = resume.Migrate(KeyMigrationOptions{DestinationBackend: KeyBackendSystem, SystemLocator: testSystemLocator, SystemKeys: provider})
		if !errors.Is(err, ErrKeyMigrationMismatch) {
			t.Fatalf("mismatch error=%v", err)
		}
		_ = resume.Close()
		metadata, err := InspectKey(t.Context(), root, binding)
		if err != nil || metadata.Backend != KeyBackendPassphrase {
			t.Fatalf("authority switched after mismatch: %+v %v", metadata, err)
		}
	})
}

func TestKeyMigrationSourceDeleteFailureResumesFromDestinationAuthority(t *testing.T) {
	root := filepath.Join(t.TempDir(), "offline")
	binding := testBindingValue(t)
	trust := testTrustValue(t)
	sourceSecret := bytes.Repeat([]byte{0x21}, 32)
	store, err := CreateSystem(t.Context(), root, CreateOptions{Binding: binding, TrustState: trust}, sourceSecret)
	if err != nil {
		t.Fatal(err)
	}
	binding = store.Binding()
	pack := testPack(testPackID, "SOURCE_DELETE_SECRET")
	if err := store.SavePack(t.Context(), pack); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	provider := newFakeSystemKeyProvider()
	provider.secrets[testSystemLocator] = append([]byte(nil), sourceSecret...)
	provider.deleteErr = errors.New("delete failed")
	destinationPassphrase := []byte("replacement passphrase for migration")
	session, err := BeginKeyMigration(t.Context(), root, binding, trust, sourceSecret, KeyBackendSystem)
	if err != nil {
		t.Fatal(err)
	}
	_, err = session.Migrate(KeyMigrationOptions{
		DestinationBackend: KeyBackendPassphrase, SystemLocator: testSystemLocator,
		DestinationPassphrase: destinationPassphrase, SystemKeys: provider,
	})
	if !errors.Is(err, ErrKeyBackendUnavailable) {
		t.Fatalf("delete failure=%v", err)
	}
	_ = session.Close()
	metadata, err := InspectKey(t.Context(), root, binding)
	if err != nil || metadata.Backend != KeyBackendPassphrase {
		t.Fatalf("destination authority was not durable: %+v %v", metadata, err)
	}
	if sourceBackend, markerErr := migrationSourceBackend(root); markerErr != nil || sourceBackend != KeyBackendSystem {
		t.Fatalf("system source cleanup responsibility was not durable: backend=%d err=%v", sourceBackend, markerErr)
	}
	if opened, openErr := OpenWithBackend(t.Context(), root, binding, trust, destinationPassphrase, KeyBackendPassphrase); !errors.Is(openErr, ErrKeyMigrationPending) {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatalf("normal open after cleanup failure=%v", openErr)
	}
	provider.deleteErr = nil
	resume, err := BeginKeyMigration(t.Context(), root, binding, trust, destinationPassphrase, KeyBackendPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resume.Migrate(KeyMigrationOptions{
		DestinationBackend: KeyBackendPassphrase, SystemLocator: testSystemLocator,
		DestinationPassphrase: destinationPassphrase, SystemKeys: provider,
	}); err != nil {
		t.Fatal(err)
	}
	if err := resume.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Load(testSystemLocator); !errors.Is(err, ErrSystemKeyNotFound) {
		t.Fatalf("source system key remains: %v", err)
	}
	if _, markerErr := migrationSourceBackend(root); !errors.Is(markerErr, ErrNotFound) {
		t.Fatalf("source cleanup marker remains: %v", markerErr)
	}
	reopened, err := OpenWithBackend(t.Context(), root, binding, trust, destinationPassphrase, KeyBackendPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, err := reopened.GetPack(t.Context(), pack.ID); err != nil || !bytes.Equal(got.Canonical, pack.Canonical) {
		t.Fatalf("pack after cleanup resume=%#v %v", got, err)
	}
}

func TestKeyMigrationExclusiveLeaseSpansExternalBackendWork(t *testing.T) {
	root, binding, trust, store := createTestStore(t)
	_ = store.Close()
	provider := newFakeSystemKeyProvider()
	session, err := BeginKeyMigration(t.Context(), root, binding, trust, testPassphrase, KeyBackendPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	_, err = session.Migrate(KeyMigrationOptions{
		DestinationBackend: KeyBackendSystem, SystemLocator: testSystemLocator, SystemKeys: provider,
		Failpoint: func(point KeyMigrationFailpoint) error {
			if point != KeyMigrationAfterDestinationStore {
				return nil
			}
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
			defer cancel()
			lease, leaseErr := AcquireLease(ctx, root, LeaseShared, 25*time.Millisecond)
			if lease != nil {
				_ = lease.Close()
			}
			if !errors.Is(leaseErr, ErrProfileBusy) {
				t.Fatalf("migration released exclusive lease during backend work: %v", leaseErr)
			}
			return errMigrationCrash
		},
	})
	if !errors.Is(err, errMigrationCrash) {
		t.Fatal(err)
	}
	_ = session.Close()
}

func assertNoKeyMigrationJournal(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "objects", ObjectJournal.directory()))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != filepath.Base(objectRelative(ObjectJournal, testProfileIDFromRoot(t, root))) {
			t.Fatalf("pending journal remains: %s", entry.Name())
		}
	}
}

func testProfileIDFromRoot(t *testing.T, root string) string {
	t.Helper()
	keyFile, err := os.ReadFile(filepath.Join(root, "profile.key"))
	if err != nil {
		t.Fatal(err)
	}
	header, err := DecodeKeyHeader(keyFile[:KeyHeaderSize])
	if err != nil {
		t.Fatal(err)
	}
	return header.Binding().ProfileUUID()
}

func zeroBytesIfOwned(backend uint8, value []byte) {
	if backend == KeyBackendSystem {
		zeroBytes(value)
	}
}
