//go:build windows

package credentials

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
	"golang.org/x/sys/windows"
)

const credentialFileName = "credential.dpapi"

type FileStore struct{ Path string }

func NewFileStore(path string) Store { return FileStore{Path: path} }

func (s FileStore) Load() (Record, error) {
	protected, err := securefile.Read(s.Path, false)
	if errors.Is(err, securefile.ErrNotFound) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("read protected credential: %w", err)
	}
	record, err := decodeProtectedRecord(protected, unprotectCurrentUser)
	if err != nil {
		return Record{}, errors.New("credential could not be unprotected for the current Windows user")
	}
	return record, nil
}

func (s FileStore) Save(record Record) error {
	protected, err := encodeProtectedRecord(record, protectCurrentUser)
	if err != nil {
		return errors.New("credential could not be protected for the current Windows user")
	}
	if err := securefile.AtomicWrite(s.Path, protected, false); err != nil {
		return fmt.Errorf("save protected credential: %w", err)
	}
	return nil
}

func (s FileStore) Delete() error { return securefile.Delete(s.Path) }

func protectCurrentUser(data []byte) ([]byte, error) {
	input := dataBlob(data)
	var output windows.DataBlob
	if err := windows.CryptProtectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return nil, err
	}
	return copyAndFree(output), nil
}

func unprotectCurrentUser(data []byte) ([]byte, error) {
	input := dataBlob(data)
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return nil, err
	}
	return copyAndFree(output), nil
}

func dataBlob(data []byte) windows.DataBlob {
	if len(data) == 0 {
		return windows.DataBlob{}
	}
	return windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
}

func copyAndFree(blob windows.DataBlob) []byte {
	if blob.Data == nil || blob.Size == 0 {
		return nil
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(blob.Data))))
	return append([]byte(nil), unsafe.Slice(blob.Data, int(blob.Size))...)
}
