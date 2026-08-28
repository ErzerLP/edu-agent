package offline

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHeaderGoldenAndStrictValidation(t *testing.T) {
	var key KeyHeader
	key.Backend, key.KDFProfile, key.WrappedLength = 1, 1, 48
	for i := range key.OriginHash {
		key.OriginHash[i] = byte(i)
	}
	for i := range key.DeviceID {
		key.DeviceID[i] = byte(0x20 + i)
	}
	key.LearnerGeneration = 0x0102030405060708
	for i := range key.ProfileID {
		key.ProfileID[i] = byte(0x30 + i)
	}
	for i := range key.Salt {
		key.Salt[i] = byte(0x40 + i)
	}
	for i := range key.WrapNonce {
		key.WrapNonce[i] = byte(0x50 + i)
	}
	encodedKey, err := key.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	const keyGolden = "4544554b455931000001008001010000000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f0102030405060708303132333435363738393a3b3c3d3e3f404142434445464748494a4b4c4d4e4f505152535455565758595a5b000000300000000000000000"
	if got := hex.EncodeToString(encodedKey); got != keyGolden {
		t.Fatalf("key header golden mismatch\n got %s\nwant %s", got, keyGolden)
	}
	decodedKey, err := DecodeKeyHeader(encodedKey)
	if err != nil || decodedKey != key {
		t.Fatalf("key round trip failed: %v", err)
	}

	var binding Binding
	binding.OriginHash = key.OriginHash
	binding.DeviceID = key.DeviceID
	binding.LearnerGeneration = key.LearnerGeneration
	copy(binding.ProfileID[:], []byte{0x40, 0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x88, 0x49, 0x4a, 0x4b, 0x4c, 0x4d, 0x4e, 0x4f})
	var logical [16]byte
	copy(logical[:], []byte{0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x46, 0x37, 0x88, 0x39, 0x3a, 0x3b, 0x3c, 0x3d, 0x3e, 0x3f})
	var nonce [12]byte
	for i := range nonce {
		nonce[i] = byte(0x50 + i)
	}
	object := NewObjectHeader(binding, ObjectOperation, logical, nonce, 32)
	encodedObject, err := object.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	const objectGolden = "4544554f46463100000100a0000000000000000000000020000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f010203040506070803010000303132333435463788393a3b3c3d3e3f404142434445464788494a4b4c4d4e4f505152535455565758595a5b0000000000000000000000000000000000000000000000000000000000000000"
	if got := hex.EncodeToString(encodedObject); got != objectGolden {
		t.Fatalf("object header golden mismatch\n got %s\nwant %s", got, objectGolden)
	}
	decodedObject, err := DecodeObjectHeader(encodedObject)
	if err != nil || decodedObject != object {
		t.Fatalf("object round trip failed: %v", err)
	}

	for name, mutate := range map[string]func([]byte){
		"key magic":       func(b []byte) { b[0] ^= 1 },
		"key reserved":    func(b []byte) { b[120] = 1 },
		"object reserved": func(b []byte) { b[159] = 1 },
		"object flags":    func(b []byte) { b[15] = 1 },
		"object oversize": func(b []byte) {
			for i := 16; i < 24; i++ {
				b[i] = 0xff
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			if strings.HasPrefix(name, "key") {
				value := append([]byte(nil), encodedKey...)
				mutate(value)
				if _, err := DecodeKeyHeader(value); err == nil {
					t.Fatal("expected strict key rejection")
				}
			} else {
				value := append([]byte(nil), encodedObject...)
				mutate(value)
				if _, err := DecodeObjectHeader(value); err == nil {
					t.Fatal("expected strict object rejection")
				}
			}
		})
	}
	if _, err := DecodeKeyHeader(encodedKey[:127]); err == nil {
		t.Fatal("expected truncated key rejection")
	}
	if _, err := DecodeObjectHeader(encodedObject[:159]); err == nil {
		t.Fatal("expected truncated object rejection")
	}
}

func TestWrongPassphraseTrustBindingAndAuthenticatedObjectFailures(t *testing.T) {
	root, binding, trust, store := createTestStore(t)
	if err := store.SavePack(context.Background(), testPack(testPackID, "sensitive prompt")); err != nil {
		t.Fatal(err)
	}
	fullBinding := store.Binding()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := OpenPassphrase(context.Background(), root, binding, trust, []byte("wrong passphrase"))
	requireErrorIs(t, err, ErrKeyUnavailable)

	wrongTrust, _ := NewTrustState(json.RawMessage(`{"manifest_digest":"different","manifest_revision":"1"}`))
	_, err = OpenPassphrase(context.Background(), root, binding, wrongTrust, testPassphrase)
	requireErrorIs(t, err, ErrBindingMismatch)
	for name, wrongBinding := range map[string]Binding{
		"origin":     func() Binding { value := binding; value.OriginHash[0] ^= 1; return value }(),
		"device":     func() Binding { value := binding; value.DeviceID[0] ^= 1; return value }(),
		"generation": func() Binding { value := binding; value.LearnerGeneration++; return value }(),
		"profile":    func() Binding { value := fullBinding; value.ProfileID[0] ^= 1; return value }(),
	} {
		t.Run("binding "+name, func(t *testing.T) {
			_, err := OpenPassphrase(context.Background(), root, wrongBinding, trust, testPassphrase)
			requireErrorIs(t, err, ErrBindingMismatch)
		})
	}

	opened, err := OpenPassphrase(context.Background(), root, binding, trust, testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	path := filepath.Join(root, filepath.FromSlash(objectRelative(ObjectPack, testPackID)))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func([]byte) []byte{
		"tamper":   func(value []byte) []byte { value[len(value)-1] ^= 1; return value },
		"truncate": func(value []byte) []byte { return value[:len(value)-1] },
		"trailing": func(value []byte) []byte { return append(value, 0) },
		"reserved": func(value []byte) []byte { value[159] = 1; return value },
	} {
		t.Run(name, func(t *testing.T) {
			value := mutate(append([]byte(nil), original...))
			if err := os.WriteFile(path, value, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := opened.GetPack(context.Background(), testPackID)
			if err == nil {
				t.Fatal("expected fail-closed object read")
			}
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
		})
	}

	otherRoot := filepath.Join(t.TempDir(), "offline")
	other, err := CreatePassphrase(context.Background(), otherRoot, CreateOptions{Binding: binding, TrustState: trust}, testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	otherPath := filepath.Join(otherRoot, filepath.FromSlash(objectRelative(ObjectPack, testPackID)))
	if err := os.MkdirAll(filepath.Dir(otherPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = other.GetPack(context.Background(), testPackID)
	if !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("cross-profile copy returned %v", err)
	}
}
