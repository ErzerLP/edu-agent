package credentials

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

type protectFunc func([]byte) ([]byte, error)

func encodeProtectedRecord(record Record, protect protectFunc) ([]byte, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	plain, err := json.Marshal(record)
	if err != nil {
		return nil, errors.New("credential data could not be encoded")
	}
	defer clear(plain)
	protected, err := protect(plain)
	if err != nil {
		return nil, err
	}
	return protected, nil
}

func decodeProtectedRecord(protected []byte, unprotect protectFunc) (Record, error) {
	plain, err := unprotect(protected)
	if err != nil {
		return Record{}, err
	}
	defer clear(plain)
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	var record Record
	if err := decoder.Decode(&record); err != nil {
		return Record{}, errors.New("credential data is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Record{}, errors.New("credential data contains multiple values")
	}
	if err := record.Validate(); err != nil {
		return Record{}, err
	}
	return record, nil
}
