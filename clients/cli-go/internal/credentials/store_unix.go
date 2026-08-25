//go:build !windows

package credentials

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

const credentialFileName = "credential.json"

type FileStore struct{ Path string }

func NewFileStore(path string) Store { return FileStore{Path: path} }

func (s FileStore) Load() (Record, error) {
	data, err := securefile.Read(s.Path, true)
	if errors.Is(err, securefile.ErrNotFound) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("read credential: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
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

func (s FileStore) Save(record Record) error {
	if err := record.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return errors.New("credential data could not be encoded")
	}
	if err := securefile.AtomicWrite(s.Path, append(data, '\n'), true); err != nil {
		return fmt.Errorf("save credential: %w", err)
	}
	return nil
}

func (s FileStore) Delete() error { return securefile.Delete(s.Path) }
