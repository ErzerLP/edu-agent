package credentials

import (
	"bytes"
	"errors"
	"testing"
)

func TestProtectedCredentialCodecUsesInjectedCurrentUserBoundary(t *testing.T) {
	t.Parallel()
	record := Record{ServerURL: "https://example.test", DeviceID: "device-1", Token: "token-secret"}
	protectCalls, unprotectCalls := 0, 0
	protected, err := encodeProtectedRecord(record, func(plain []byte) ([]byte, error) {
		protectCalls++
		return append([]byte("current-user:"), plain...), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(protected[:len("current-user:")], []byte(record.Token)) {
		t.Fatal("test protector prefix unexpectedly contains token")
	}
	loaded, err := decodeProtectedRecord(protected, func(value []byte) ([]byte, error) {
		unprotectCalls++
		return append([]byte(nil), value[len("current-user:"):]...), nil
	})
	if err != nil || loaded != record || protectCalls != 1 || unprotectCalls != 1 {
		t.Fatalf("loaded=%+v err=%v protect=%d unprotect=%d", loaded, err, protectCalls, unprotectCalls)
	}
}

func TestProtectedCredentialCodecFailsClosed(t *testing.T) {
	t.Parallel()
	record := Record{ServerURL: "https://example.test", DeviceID: "device-1", Token: "token-secret"}
	if _, err := encodeProtectedRecord(record, func([]byte) ([]byte, error) { return nil, errors.New("protect failed") }); err == nil {
		t.Fatal("encodeProtectedRecord ignored protection failure")
	}
	if _, err := decodeProtectedRecord([]byte("protected"), func([]byte) ([]byte, error) { return nil, errors.New("wrong user") }); err == nil {
		t.Fatal("decodeProtectedRecord ignored current-user protection failure")
	}
}
