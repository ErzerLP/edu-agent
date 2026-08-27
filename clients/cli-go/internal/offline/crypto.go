package offline

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime        uint32 = 3
	argonMemoryKiB   uint32 = 64 * 1024
	argonParallelism uint8  = 1
	argonOutputBytes uint32 = 32
)

func createWrappedKey(binding Binding, passphrase []byte) ([]byte, []byte, error) {
	return createWrappedKeyForBackend(binding, passphrase, KeyBackendPassphrase)
}

func createWrappedKeyForBackend(binding Binding, wrappingMaterial []byte, backend uint8) ([]byte, []byte, error) {
	if len(wrappingMaterial) == 0 {
		return nil, nil, ErrKeyUnavailable
	}
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return nil, nil, fmt.Errorf("generate offline data key: %w", err)
	}
	keyFile, err := wrapExistingKeyForBackend(binding, wrappingMaterial, dek, backend)
	if err != nil {
		zeroBytes(dek)
		return nil, nil, err
	}
	return keyFile, dek, nil
}

func wrapExistingKey(binding Binding, passphrase, dek []byte) ([]byte, error) {
	return wrapExistingKeyForBackend(binding, passphrase, dek, KeyBackendPassphrase)
}

func wrapExistingKeyForBackend(binding Binding, passphrase, dek []byte, backend uint8) ([]byte, error) {
	if len(passphrase) == 0 || len(dek) != 32 {
		return nil, ErrKeyUnavailable
	}
	if backend != KeyBackendPassphrase && backend != KeyBackendSystem {
		return nil, ErrKeyUnavailable
	}
	header := KeyHeader{Backend: backend, KDFProfile: KDFArgon2idV1, OriginHash: binding.OriginHash, DeviceID: binding.DeviceID, LearnerGeneration: binding.LearnerGeneration, ProfileID: binding.ProfileID, WrappedLength: 48}
	if _, err := rand.Read(header.Salt[:]); err != nil {
		return nil, fmt.Errorf("generate offline KDF salt: %w", err)
	}
	if _, err := rand.Read(header.WrapNonce[:]); err != nil {
		return nil, fmt.Errorf("generate offline wrapping nonce: %w", err)
	}
	aad, err := header.MarshalBinary()
	if err != nil {
		return nil, err
	}
	kek := argon2.IDKey(passphrase, header.Salt[:], argonTime, argonMemoryKiB, argonParallelism, argonOutputBytes)
	defer zeroBytes(kek)
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, ErrKeyUnavailable
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrKeyUnavailable
	}
	wrapped := gcm.Seal(nil, header.WrapNonce[:], dek, aad)
	keyFile := make([]byte, 0, len(aad)+len(wrapped))
	keyFile = append(keyFile, aad...)
	keyFile = append(keyFile, wrapped...)
	return keyFile, nil
}

func unwrapKey(keyFile, passphrase []byte, expected Binding) (KeyHeader, []byte, error) {
	if len(passphrase) == 0 || len(keyFile) != KeyHeaderSize+48 {
		return KeyHeader{}, nil, ErrKeyUnavailable
	}
	header, err := DecodeKeyHeader(keyFile[:KeyHeaderSize])
	if err != nil {
		return KeyHeader{}, nil, ErrKeyUnavailable
	}
	if !header.Binding().matches(expected) {
		return KeyHeader{}, nil, ErrBindingMismatch
	}
	kek := argon2.IDKey(passphrase, header.Salt[:], argonTime, argonMemoryKiB, argonParallelism, argonOutputBytes)
	defer zeroBytes(kek)
	block, err := aes.NewCipher(kek)
	if err != nil {
		return KeyHeader{}, nil, ErrKeyUnavailable
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return KeyHeader{}, nil, ErrKeyUnavailable
	}
	dek, err := gcm.Open(nil, header.WrapNonce[:], keyFile[KeyHeaderSize:], keyFile[:KeyHeaderSize])
	if err != nil || len(dek) != 32 {
		zeroBytes(dek)
		return KeyHeader{}, nil, ErrKeyUnavailable
	}
	return header, dek, nil
}

func sealContainer(dek []byte, header ObjectHeader, plaintext []byte) ([]byte, error) {
	if len(dek) != 32 || len(plaintext) > MaxSealedObject-16 {
		return nil, fmt.Errorf("%w: object plaintext size", ErrCorruptStore)
	}
	header.SealedLength = uint64(len(plaintext) + 16)
	aad, err := header.MarshalBinary()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, ErrKeyUnavailable
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrKeyUnavailable
	}
	sealed := gcm.Seal(nil, header.Nonce[:], plaintext, aad)
	container := make([]byte, 0, len(aad)+len(sealed))
	container = append(container, aad...)
	container = append(container, sealed...)
	return container, nil
}

func openContainer(dek, container []byte, expected Binding, kind ObjectKind, logicalID [16]byte) (ObjectHeader, []byte, error) {
	if len(container) < ObjectHeaderSize {
		return ObjectHeader{}, nil, fmt.Errorf("%w: truncated object header", ErrCorruptStore)
	}
	header, err := DecodeObjectHeader(container[:ObjectHeaderSize])
	if err != nil {
		return ObjectHeader{}, nil, err
	}
	if err := header.ValidateBinding(expected, kind, logicalID); err != nil {
		return ObjectHeader{}, nil, err
	}
	if header.SealedLength != uint64(len(container)-ObjectHeaderSize) {
		return ObjectHeader{}, nil, fmt.Errorf("%w: object length mismatch", ErrCorruptStore)
	}
	if len(dek) != 32 {
		return ObjectHeader{}, nil, ErrKeyUnavailable
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return ObjectHeader{}, nil, ErrKeyUnavailable
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return ObjectHeader{}, nil, ErrKeyUnavailable
	}
	plain, err := gcm.Open(nil, header.Nonce[:], container[ObjectHeaderSize:], container[:ObjectHeaderSize])
	if err != nil {
		return ObjectHeader{}, nil, fmt.Errorf("%w: authenticated object open failed", ErrCorruptStore)
	}
	return header, plain, nil
}

func nonceFor(prefix [4]byte, counter uint64) ([12]byte, error) {
	var nonce [12]byte
	if prefix == ([4]byte{}) || counter == 0 {
		return nonce, errors.New("offline nonce prefix and counter must be non-zero")
	}
	copy(nonce[:4], prefix[:])
	binary.BigEndian.PutUint64(nonce[4:], counter)
	return nonce, nil
}

func nonceParts(nonce [12]byte) ([4]byte, uint64) {
	var prefix [4]byte
	copy(prefix[:], nonce[:4])
	return prefix, binary.BigEndian.Uint64(nonce[4:])
}

func randomNoncePrefix() ([4]byte, error) {
	var prefix [4]byte
	for prefix == ([4]byte{}) {
		if _, err := rand.Read(prefix[:]); err != nil {
			return prefix, err
		}
	}
	return prefix, nil
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
