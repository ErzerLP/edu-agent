package credentials

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var ErrNotFound = errors.New("credential was not found")

type Record struct {
	ServerURL string `json:"server_url"`
	DeviceID  string `json:"device_id"`
	Token     string `json:"token"`
}

func (r Record) Validate() error {
	if r.ServerURL == "" || r.DeviceID == "" || r.Token == "" {
		return errors.New("credential binding is incomplete")
	}
	return nil
}

type Store interface {
	Load() (Record, error)
	Save(Record) error
	Delete() error
}

func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user configuration: %w", err)
	}
	return filepath.Join(dir, "edu-agent", credentialFileName), nil
}
