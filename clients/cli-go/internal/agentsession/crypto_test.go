package agentsession

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenContainerClassifiesAuthenticatedVersionsAndHeaderTamper(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	profile := [32]byte{1, 2, 3}
	_, session, err := randomUUID()
	if err != nil {
		t.Fatal(err)
	}
	_, storage, err := randomStorageID()
	if err != nil {
		t.Fatal(err)
	}
	header := containerHeader{
		SchemaVersion: 1,
		Kind:          kindRecord,
		Profile:       profile,
		Generation:    7,
		Session:       session,
		Storage:       storage,
		Revision:      9,
	}
	expected := containerExpectation{
		SchemaVersion: 1,
		Kind:          kindRecord,
		Profile:       profile,
		Generation:    7,
		Session:       session,
		Storage:       storage,
		Revision:      9,
		MaxPayload:    1024,
	}
	plaintext := []byte(`{"value":"secret"}`)
	current, err := sealContainer(key, header, plaintext)
	if err != nil {
		t.Fatal(err)
	}

	futureContainer := sealContainerVersionForTest(t, key, header, containerVersion+1, 1, plaintext)
	if _, _, err := openContainer(key, futureContainer, expected); !errors.Is(err, ErrVersionUnsupported) {
		t.Fatalf("future container version error=%v", err)
	}
	futurePayload := sealContainerVersionForTest(t, key, header, containerVersion, 2, plaintext)
	if _, _, err := openContainer(key, futurePayload, expected); !errors.Is(err, ErrVersionUnsupported) {
		t.Fatalf("future payload schema error=%v", err)
	}
	authenticatedKindMismatch := header
	authenticatedKindMismatch.Kind = kindDirty
	if _, _, err := openContainer(key, sealContainerVersionForTest(t, key, authenticatedKindMismatch, containerVersion, 1, plaintext), expected); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("authenticated kind mismatch error=%v", err)
	}

	for name, mutate := range map[string]func([]byte){
		"container-version": func(value []byte) { binary.BigEndian.PutUint16(value[8:10], containerVersion+1) },
		"payload-schema":    func(value []byte) { binary.BigEndian.PutUint16(value[10:12], 2) },
		"record-kind":       func(value []byte) { value[12] = byte(kindDirty) },
		"profile":           func(value []byte) { value[16] ^= 1 },
		"generation":        func(value []byte) { binary.BigEndian.PutUint64(value[48:56], 8) },
		"revision": func(value []byte) {
			binary.BigEndian.PutUint64(value[88:96], 10)
			binary.BigEndian.PutUint64(value[100:108], 10)
		},
	} {
		t.Run(name, func(t *testing.T) {
			tampered := append([]byte(nil), current...)
			mutate(tampered)
			if _, _, err := openContainer(key, tampered, expected); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("authenticated header tamper error=%v", err)
			}
		})
	}

	if code := StableErrorCode(ErrVersionUnsupported); code != ErrorCodeVersionUnsupported {
		t.Fatalf("version code=%q", code)
	}
	if code := StableErrorCode(ErrCorrupt); code != ErrorCodeCorrupt {
		t.Fatalf("corrupt code=%q", code)
	}
}

func TestStoreClassifiesFutureRecordPayloadSchema(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "future", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	future := cloneRecord(record)
	future.SchemaVersion = recordPayloadSchemaVersion + 1
	plain, err := encodeStrict(future)
	if err != nil {
		t.Fatal(err)
	}
	session, err := parseUUID(record.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := parseStorageID(record.StorageID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := sealContainer(handle.dataKey, containerHeader{
		SchemaVersion: recordContainerSchemaVersion,
		Kind:          kindRecord,
		Profile:       store.profile,
		Generation:    record.PrivacyGeneration,
		Session:       session,
		Storage:       storage,
		Revision:      record.RecordRevision,
	}, plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.rootPath, recordName(record.StorageID)), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Load(); !errors.Is(err, ErrVersionUnsupported) {
		t.Fatalf("future record payload error=%v", err)
	}
}

func TestFutureIndexContainerIsNotTreatedAsEmptyStore(t *testing.T) {
	store := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	defer store.Close()
	if listed, err := store.List(t.Context()); err != nil || len(listed) != 0 {
		t.Fatalf("initial list=%+v err=%v", listed, err)
	}
	snapshot, err := store.root.ReadSnapshot(indexName, store.limits.SessionCiphertextBytes, true)
	if err != nil {
		t.Fatal(err)
	}
	store.stateMu.RLock()
	profileKey := append([]byte(nil), store.profileKey...)
	generation := store.generation
	store.stateMu.RUnlock()
	defer zero(profileKey)
	plain, header, err := openContainer(profileKey, snapshot.Data, containerExpectation{
		SchemaVersion: indexSchemaVersion,
		Kind:          kindIndex,
		Profile:       store.profile,
		Generation:    generation,
		MaxPayload:    store.limits.SessionPlaintextBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	future := sealContainerVersionForTest(t, profileKey, header, containerVersion+1, indexSchemaVersion, plain)
	if err := os.WriteFile(filepath.Join(store.rootPath, indexName), future, 0o600); err != nil {
		t.Fatal(err)
	}
	if listed, err := store.List(t.Context()); !errors.Is(err, ErrVersionUnsupported) || listed != nil {
		t.Fatalf("future index list=%+v err=%v", listed, err)
	}
}

func TestSealContainerUsesUniqueNonZeroNonces(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	_, session, err := randomUUID()
	if err != nil {
		t.Fatal(err)
	}
	_, storage, err := randomStorageID()
	if err != nil {
		t.Fatal(err)
	}
	header := containerHeader{
		SchemaVersion: 1,
		Kind:          kindRecord,
		Profile:       [32]byte{1, 2, 3},
		Generation:    1,
		Session:       session,
		Storage:       storage,
		Revision:      1,
	}
	seen := make(map[[12]byte]struct{}, 1024)
	for index := 0; index < 1024; index++ {
		encoded, err := sealContainer(key, header, []byte(`{"value":"secret"}`))
		if err != nil {
			t.Fatalf("seal %d: %v", index, err)
		}
		parsed, err := unmarshalHeader(encoded[:containerHeaderSize])
		if err != nil {
			t.Fatalf("header %d: %v", index, err)
		}
		if parsed.Nonce == ([12]byte{}) {
			t.Fatalf("nonce %d was all zero", index)
		}
		if _, duplicate := seen[parsed.Nonce]; duplicate {
			t.Fatalf("nonce %d was reused: %x", index, parsed.Nonce)
		}
		seen[parsed.Nonce] = struct{}{}
	}
}

func sealContainerVersionForTest(t *testing.T, key []byte, header containerHeader, version, schema uint16, plaintext []byte) []byte {
	t.Helper()
	header.ContainerVersion = version
	header.SchemaVersion = schema
	binary.BigEndian.PutUint32(header.Nonce[:4], uint32(version)<<16|uint32(schema))
	binary.BigEndian.PutUint64(header.Nonce[4:], header.Revision)
	derivedKey := deriveContainerKey(key, header)
	defer zero(derivedKey)
	block, err := aes.NewCipher(derivedKey)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	header.CipherLength = uint64(len(plaintext) + gcm.Overhead())
	rawHeader, err := marshalHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	return append(rawHeader, gcm.Seal(nil, header.Nonce[:], plaintext, rawHeader)...)
}
