package offline

import (
	"encoding/binary"
	"errors"
	"fmt"
)

var (
	keyMagic    = [8]byte{'E', 'D', 'U', 'K', 'E', 'Y', '1', 0}
	objectMagic = [8]byte{'E', 'D', 'U', 'O', 'F', 'F', '1', 0}
)

const (
	KeyBackendPassphrase uint8 = 1
	KeyBackendSystem     uint8 = 2
	KDFArgon2idV1        uint8 = 1
)

type KeyHeader struct {
	Backend           uint8
	KDFProfile        uint8
	OriginHash        [32]byte
	DeviceID          [16]byte
	LearnerGeneration uint64
	ProfileID         [16]byte
	Salt              [16]byte
	WrapNonce         [12]byte
	WrappedLength     uint32
}

func (h KeyHeader) Binding() Binding {
	return Binding{OriginHash: h.OriginHash, DeviceID: h.DeviceID, LearnerGeneration: h.LearnerGeneration, ProfileID: h.ProfileID}
}

func (h KeyHeader) MarshalBinary() ([]byte, error) {
	if (h.Backend != KeyBackendPassphrase && h.Backend != KeyBackendSystem) || h.KDFProfile != KDFArgon2idV1 || h.WrappedLength != 48 {
		return nil, errors.New("unsupported offline key header")
	}
	if err := h.Binding().validate(true); err != nil {
		return nil, err
	}
	if h.Salt == ([16]byte{}) || h.WrapNonce == ([12]byte{}) {
		return nil, errors.New("offline key header randomness is missing")
	}
	out := make([]byte, KeyHeaderSize)
	copy(out[0:8], keyMagic[:])
	binary.BigEndian.PutUint16(out[8:10], 1)
	binary.BigEndian.PutUint16(out[10:12], KeyHeaderSize)
	out[12] = h.Backend
	out[13] = h.KDFProfile
	copy(out[16:48], h.OriginHash[:])
	copy(out[48:64], h.DeviceID[:])
	binary.BigEndian.PutUint64(out[64:72], h.LearnerGeneration)
	copy(out[72:88], h.ProfileID[:])
	copy(out[88:104], h.Salt[:])
	copy(out[104:116], h.WrapNonce[:])
	binary.BigEndian.PutUint32(out[116:120], h.WrappedLength)
	return out, nil
}

func DecodeKeyHeader(raw []byte) (KeyHeader, error) {
	var h KeyHeader
	if len(raw) != KeyHeaderSize {
		return h, fmt.Errorf("%w: key header size", ErrCorruptStore)
	}
	if string(raw[0:8]) != string(keyMagic[:]) || binary.BigEndian.Uint16(raw[8:10]) != 1 || binary.BigEndian.Uint16(raw[10:12]) != KeyHeaderSize {
		return h, fmt.Errorf("%w: key header magic or version", ErrCorruptStore)
	}
	if raw[14] != 0 || raw[15] != 0 || !allZero(raw[120:128]) {
		return h, fmt.Errorf("%w: key header reserved bytes", ErrCorruptStore)
	}
	h.Backend = raw[12]
	h.KDFProfile = raw[13]
	copy(h.OriginHash[:], raw[16:48])
	copy(h.DeviceID[:], raw[48:64])
	h.LearnerGeneration = binary.BigEndian.Uint64(raw[64:72])
	copy(h.ProfileID[:], raw[72:88])
	copy(h.Salt[:], raw[88:104])
	copy(h.WrapNonce[:], raw[104:116])
	h.WrappedLength = binary.BigEndian.Uint32(raw[116:120])
	if (h.Backend != KeyBackendPassphrase && h.Backend != KeyBackendSystem) || h.KDFProfile != KDFArgon2idV1 || h.WrappedLength != 48 || h.Salt == ([16]byte{}) || h.WrapNonce == ([12]byte{}) {
		return KeyHeader{}, fmt.Errorf("%w: unsupported key backend or KDF", ErrCorruptStore)
	}
	if err := h.Binding().validate(true); err != nil {
		return KeyHeader{}, fmt.Errorf("%w: invalid key binding", ErrCorruptStore)
	}
	return h, nil
}

type ObjectHeader struct {
	Flags             uint32
	SealedLength      uint64
	OriginHash        [32]byte
	DeviceID          [16]byte
	LearnerGeneration uint64
	Kind              ObjectKind
	Schema            uint8
	Compression       uint8
	LogicalID         [16]byte
	ProfileID         [16]byte
	Nonce             [12]byte
}

func NewObjectHeader(binding Binding, kind ObjectKind, logicalID [16]byte, nonce [12]byte, sealedLength uint64) ObjectHeader {
	return ObjectHeader{SealedLength: sealedLength, OriginHash: binding.OriginHash, DeviceID: binding.DeviceID, LearnerGeneration: binding.LearnerGeneration, Kind: kind, Schema: 1, LogicalID: logicalID, ProfileID: binding.ProfileID, Nonce: nonce}
}

func (h ObjectHeader) Binding() Binding {
	return Binding{OriginHash: h.OriginHash, DeviceID: h.DeviceID, LearnerGeneration: h.LearnerGeneration, ProfileID: h.ProfileID}
}

func (h ObjectHeader) MarshalBinary() ([]byte, error) {
	if err := h.validate(); err != nil {
		return nil, err
	}
	out := make([]byte, ObjectHeaderSize)
	copy(out[0:8], objectMagic[:])
	binary.BigEndian.PutUint16(out[8:10], 1)
	binary.BigEndian.PutUint16(out[10:12], ObjectHeaderSize)
	binary.BigEndian.PutUint32(out[12:16], h.Flags)
	binary.BigEndian.PutUint64(out[16:24], h.SealedLength)
	copy(out[24:56], h.OriginHash[:])
	copy(out[56:72], h.DeviceID[:])
	binary.BigEndian.PutUint64(out[72:80], h.LearnerGeneration)
	out[80] = byte(h.Kind)
	out[81] = h.Schema
	out[82] = h.Compression
	copy(out[84:100], h.LogicalID[:])
	copy(out[100:116], h.ProfileID[:])
	copy(out[116:128], h.Nonce[:])
	return out, nil
}

func DecodeObjectHeader(raw []byte) (ObjectHeader, error) {
	var h ObjectHeader
	if len(raw) != ObjectHeaderSize {
		return h, fmt.Errorf("%w: object header size", ErrCorruptStore)
	}
	if string(raw[0:8]) != string(objectMagic[:]) || binary.BigEndian.Uint16(raw[8:10]) != 1 || binary.BigEndian.Uint16(raw[10:12]) != ObjectHeaderSize {
		return h, fmt.Errorf("%w: object header magic or version", ErrCorruptStore)
	}
	h.Flags = binary.BigEndian.Uint32(raw[12:16])
	h.SealedLength = binary.BigEndian.Uint64(raw[16:24])
	copy(h.OriginHash[:], raw[24:56])
	copy(h.DeviceID[:], raw[56:72])
	h.LearnerGeneration = binary.BigEndian.Uint64(raw[72:80])
	h.Kind = ObjectKind(raw[80])
	h.Schema = raw[81]
	h.Compression = raw[82]
	if raw[83] != 0 || !allZero(raw[128:160]) {
		return ObjectHeader{}, fmt.Errorf("%w: object header reserved bytes", ErrCorruptStore)
	}
	copy(h.LogicalID[:], raw[84:100])
	copy(h.ProfileID[:], raw[100:116])
	copy(h.Nonce[:], raw[116:128])
	if err := h.validate(); err != nil {
		return ObjectHeader{}, fmt.Errorf("%w: %v", ErrCorruptStore, err)
	}
	return h, nil
}

func (h ObjectHeader) validate() error {
	if h.Flags != 0 || h.SealedLength < 16 || h.SealedLength > MaxSealedObject || !h.Kind.valid() || h.Schema != 1 || h.Compression != 0 {
		return errors.New("unsupported object header values")
	}
	if err := h.Binding().validate(true); err != nil {
		return err
	}
	if h.LogicalID == ([16]byte{}) || h.Nonce == ([12]byte{}) {
		return errors.New("object identity or nonce is missing")
	}
	return nil
}

func (h ObjectHeader) ValidateBinding(expected Binding, kind ObjectKind, logicalID [16]byte) error {
	if !h.Binding().matches(expected) || h.Kind != kind || h.LogicalID != logicalID {
		return ErrBindingMismatch
	}
	return nil
}

func allZero(value []byte) bool {
	var combined byte
	for _, current := range value {
		combined |= current
	}
	return combined == 0
}
