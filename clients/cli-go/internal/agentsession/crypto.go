package agentsession

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

const (
	containerVersion    = 1
	containerHeaderSize = 116
)

var containerMagic = [8]byte{'E', 'A', 'S', 'E', 'S', 'S', '0', '1'}

type payloadKind uint8

const (
	kindIndex payloadKind = iota + 1
	kindEnvelope
	kindRecord
	kindDirty
	kindProjection
)

type containerHeader struct {
	ContainerVersion uint16
	SchemaVersion    uint16
	Kind             payloadKind
	Profile          [32]byte
	Generation       uint64
	Session          [16]byte
	Storage          [16]byte
	Revision         uint64
	Nonce            [12]byte
	CipherLength     uint64
}

type containerExpectation struct {
	SchemaVersion uint16
	Kind          payloadKind
	Profile       [32]byte
	Generation    uint64
	Session       [16]byte
	Storage       [16]byte
	Revision      uint64
	MaxPayload    int64
}

// sealContainer authenticates the fixed header as AAD and encrypts one
// versioned session-store payload.
func sealContainer(key []byte, header containerHeader, plaintext []byte) ([]byte, error) {
	if len(key) != 32 || header.SchemaVersion == 0 || header.Kind < kindIndex || header.Kind > kindProjection || header.Generation == 0 || header.Revision == 0 {
		return nil, ErrInvalid
	}
	header.ContainerVersion = containerVersion
	if _, err := io.ReadFull(rand.Reader, header.Nonce[:4]); err != nil {
		return nil, err
	}
	binary.BigEndian.PutUint64(header.Nonce[4:], header.Revision)
	derivedKey := deriveContainerKey(key, header)
	block, err := aes.NewCipher(derivedKey)
	clear(derivedKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	header.CipherLength = uint64(len(plaintext) + gcm.Overhead())
	rawHeader, err := marshalHeader(header)
	if err != nil {
		return nil, err
	}
	ciphertext := gcm.Seal(nil, header.Nonce[:], plaintext, rawHeader)
	return append(rawHeader, ciphertext...), nil
}

// openContainer authenticates identity bindings before returning plaintext.
func openContainer(key, encoded []byte, expected containerExpectation) ([]byte, containerHeader, error) {
	if len(key) != 32 || expected.SchemaVersion == 0 || expected.Kind < kindIndex || expected.Kind > kindProjection || len(encoded) < containerHeaderSize {
		return nil, containerHeader{}, ErrCorrupt
	}
	header, err := unmarshalHeader(encoded[:containerHeaderSize])
	if err != nil {
		return nil, containerHeader{}, err
	}
	if header.CipherLength > uint64(len(encoded)-containerHeaderSize) || header.CipherLength != uint64(len(encoded)-containerHeaderSize) {
		return nil, containerHeader{}, ErrCorrupt
	}
	if expected.MaxPayload >= 0 && header.CipherLength > uint64(expected.MaxPayload+32) {
		return nil, containerHeader{}, ErrStoreFull
	}
	derivedKey := deriveContainerKey(key, header)
	block, err := aes.NewCipher(derivedKey)
	clear(derivedKey)
	if err != nil {
		return nil, containerHeader{}, ErrCorrupt
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, containerHeader{}, ErrCorrupt
	}
	plaintext, err := gcm.Open(nil, header.Nonce[:], encoded[containerHeaderSize:], encoded[:containerHeaderSize])
	if err != nil {
		return nil, containerHeader{}, ErrCorrupt
	}
	// Identity and compatibility are classified only after successful AEAD
	// authentication. Header bit flips therefore remain corruption, while an
	// authentically written future version stays distinguishable.
	if header.Kind != expected.Kind || header.Profile != expected.Profile {
		return nil, header, ErrCorrupt
	}
	if expected.Generation != 0 && header.Generation != expected.Generation {
		return nil, header, ErrPrivacyInvalidated
	}
	if expected.Session != ([16]byte{}) && header.Session != expected.Session || expected.Storage != ([16]byte{}) && header.Storage != expected.Storage || expected.Revision != 0 && header.Revision != expected.Revision {
		return nil, header, ErrCorrupt
	}
	if header.ContainerVersion != containerVersion || header.SchemaVersion != expected.SchemaVersion {
		return nil, header, ErrVersionUnsupported
	}
	if expected.MaxPayload >= 0 && int64(len(plaintext)) > expected.MaxPayload {
		return nil, containerHeader{}, ErrStoreFull
	}
	return plaintext, header, nil
}

func deriveContainerKey(key []byte, header containerHeader) []byte {
	salt := make([]byte, 0, len(header.Profile)+8+len(header.Session)+len(header.Storage))
	salt = append(salt, header.Profile[:]...)
	var generation [8]byte
	binary.BigEndian.PutUint64(generation[:], header.Generation)
	salt = append(salt, generation[:]...)
	salt = append(salt, header.Session[:]...)
	salt = append(salt, header.Storage[:]...)
	info := append([]byte("edu-agent/session/container-key/v1/"), byte(header.Kind))
	derived := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, key, salt, info), derived); err != nil {
		clear(derived)
		return nil
	}
	return derived
}

func marshalHeader(header containerHeader) ([]byte, error) {
	version := header.ContainerVersion
	if version == 0 {
		version = containerVersion
	}
	var buffer bytes.Buffer
	buffer.Grow(containerHeaderSize)
	buffer.Write(containerMagic[:])
	_ = binary.Write(&buffer, binary.BigEndian, version)
	_ = binary.Write(&buffer, binary.BigEndian, header.SchemaVersion)
	buffer.WriteByte(byte(header.Kind))
	buffer.WriteByte(0)
	_ = binary.Write(&buffer, binary.BigEndian, uint16(0))
	buffer.Write(header.Profile[:])
	_ = binary.Write(&buffer, binary.BigEndian, header.Generation)
	buffer.Write(header.Session[:])
	buffer.Write(header.Storage[:])
	_ = binary.Write(&buffer, binary.BigEndian, header.Revision)
	buffer.Write(header.Nonce[:])
	_ = binary.Write(&buffer, binary.BigEndian, header.CipherLength)
	if buffer.Len() != containerHeaderSize {
		return nil, errors.New("agent session container header size mismatch")
	}
	return buffer.Bytes(), nil
}

func unmarshalHeader(value []byte) (containerHeader, error) {
	var header containerHeader
	if len(value) != containerHeaderSize || !bytes.Equal(value[:8], containerMagic[:]) {
		return header, ErrCorrupt
	}
	reader := bytes.NewReader(value[8:])
	var kind, flags uint8
	var reserved uint16
	if binary.Read(reader, binary.BigEndian, &header.ContainerVersion) != nil || binary.Read(reader, binary.BigEndian, &header.SchemaVersion) != nil || binary.Read(reader, binary.BigEndian, &kind) != nil || binary.Read(reader, binary.BigEndian, &flags) != nil || binary.Read(reader, binary.BigEndian, &reserved) != nil {
		return header, ErrCorrupt
	}
	if header.ContainerVersion == 0 || flags != 0 || reserved != 0 {
		return header, ErrCorrupt
	}
	header.Kind = payloadKind(kind)
	if _, err := io.ReadFull(reader, header.Profile[:]); err != nil {
		return header, ErrCorrupt
	}
	if err := binary.Read(reader, binary.BigEndian, &header.Generation); err != nil {
		return header, ErrCorrupt
	}
	if _, err := io.ReadFull(reader, header.Session[:]); err != nil {
		return header, ErrCorrupt
	}
	if _, err := io.ReadFull(reader, header.Storage[:]); err != nil {
		return header, ErrCorrupt
	}
	if err := binary.Read(reader, binary.BigEndian, &header.Revision); err != nil {
		return header, ErrCorrupt
	}
	if _, err := io.ReadFull(reader, header.Nonce[:]); err != nil {
		return header, ErrCorrupt
	}
	if err := binary.Read(reader, binary.BigEndian, &header.CipherLength); err != nil || reader.Len() != 0 {
		return header, ErrCorrupt
	}
	if header.SchemaVersion == 0 || header.Kind < kindIndex || header.Kind > kindProjection || header.Generation == 0 || header.Revision == 0 || binary.BigEndian.Uint64(header.Nonce[4:]) != header.Revision {
		return header, fmt.Errorf("%w: invalid header fields", ErrCorrupt)
	}
	return header, nil
}
