package operations

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
)

const (
	AttestationAlgorithm = "hmac-sha256"
	attestationKeyBytes  = 32
	evidenceDomain       = "edu-agent.operations.evidence.attestation/v1"
	indexDomain          = "edu-agent.operations.candidate-index.attestation/v1"
	keyIDDomain          = "edu-agent.operations.attestation-key-id/v1"
)

type Attestation struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	Signature string `json:"signature"`
}

type Attestor struct {
	key   []byte
	keyID string
	path  string
}

func NewAttestor(key []byte) (*Attestor, error) {
	if len(key) != attestationKeyBytes {
		return nil, fmt.Errorf("attestation key must be %d bytes", attestationKeyBytes)
	}
	keyCopy := append([]byte(nil), key...)
	identifier := sha256.Sum256(append([]byte(keyIDDomain+"\x00"), keyCopy...))
	return &Attestor{key: keyCopy, keyID: hex.EncodeToString(identifier[:])}, nil
}

func DefaultAttestationKeyPath() (string, error) {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home for attestation key: %w", err)
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(stateHome) {
		return "", errors.New("XDG_STATE_HOME must be absolute")
	}
	return filepath.Join(stateHome, "edu-agent", "operations", "attestation.key"), nil
}

func LoadOrCreateAttestor(explicitPath, repositoryRoot, evidenceDir string) (*Attestor, string, error) {
	path, err := resolveAttestationKeyPath(explicitPath, repositoryRoot, evidenceDir)
	if err != nil {
		return nil, "", err
	}
	if err := createAttestationKeyIfMissing(path); err != nil {
		return nil, "", err
	}
	attestor, err := loadAttestor(path)
	return attestor, path, err
}

func LoadAttestor(explicitPath, repositoryRoot, evidenceDir string) (*Attestor, string, error) {
	path, err := resolveAttestationKeyPath(explicitPath, repositoryRoot, evidenceDir)
	if err != nil {
		return nil, "", err
	}
	attestor, err := loadAttestor(path)
	return attestor, path, err
}

func resolveAttestationKeyPath(explicitPath, repositoryRoot, evidenceDir string) (string, error) {
	path := explicitPath
	var err error
	if path == "" {
		path, err = DefaultAttestationKeyPath()
		if err != nil {
			return "", err
		}
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	for _, forbidden := range []struct {
		name string
		path string
	}{{"repository", repositoryRoot}, {"evidence directory", evidenceDir}} {
		if forbidden.path == "" {
			continue
		}
		inside, insideErr := pathWithin(path, forbidden.path)
		if insideErr != nil {
			return "", insideErr
		}
		if inside {
			return "", fmt.Errorf("attestation key must be outside the %s", forbidden.name)
		}
	}
	return path, nil
}

func pathWithin(path, root string) (bool, error) {
	resolvedPath, err := resolvePotentialPath(path)
	if err != nil {
		return false, err
	}
	resolvedRoot, err := resolvePotentialPath(root)
	if err != nil {
		return false, err
	}
	return lexicalPathWithin(resolvedPath, resolvedRoot), nil
}

func resolvePotentialPath(path string) (string, error) {
	current, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var suffix []string
	for {
		resolved, resolveErr := filepath.EvalSymlinks(current)
		if resolveErr == nil {
			for position := range slices.Backward(suffix) {
				resolved = filepath.Join(resolved, suffix[position])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(resolveErr, os.ErrNotExist) {
			return "", resolveErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", resolveErr
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func lexicalPathWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !filepath.IsAbs(relative) && !startsWithParent(relative)
}

func startsWithParent(path string) bool {
	return len(path) > 3 && path[:3] == ".."+string(filepath.Separator)
}

func createAttestationKeyIfMissing(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	key := make([]byte, attestationKeyBytes)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return fmt.Errorf("generate attestation key: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".attestation-key-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := temporary.Write(key); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	if err := os.Link(temporaryPath, path); err != nil && !errors.Is(err, os.ErrExist) {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("publish attestation key: %w", err)
	}
	_ = os.Remove(temporaryPath)
	if directoryHandle, openErr := os.Open(directory); openErr == nil {
		_ = directoryHandle.Sync()
		_ = directoryHandle.Close()
	}
	return nil
}

func loadAttestor(path string) (*Attestor, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("read attestation key metadata: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return nil, errors.New("attestation key must be a non-symlink regular file with mode 0600")
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read attestation key: %w", err)
	}
	attestor, err := NewAttestor(key)
	if err != nil {
		return nil, err
	}
	attestor.path = path
	return attestor, nil
}

func (attestor *Attestor) signature(domain string, value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, attestor.key)
	_, _ = mac.Write([]byte(domain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(encoded)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func (attestor *Attestor) verify(domain string, attestation Attestation, unsigned func() (any, error)) error {
	if attestor == nil {
		return errors.New("attestor is required")
	}
	if attestation.Algorithm != AttestationAlgorithm || attestation.KeyID != attestor.keyID || len(attestation.Signature) != 64 || !isLowerHex(attestation.Signature) {
		return errors.New("attestation metadata is invalid or belongs to a different key")
	}
	value, err := unsigned()
	if err != nil {
		return err
	}
	expected, err := attestor.signature(domain, value)
	if err != nil {
		return err
	}
	provided, err := hex.DecodeString(attestation.Signature)
	if err != nil {
		return errors.New("attestation signature is invalid")
	}
	expectedBytes, _ := hex.DecodeString(expected)
	if !hmac.Equal(provided, expectedBytes) {
		return errors.New("attestation signature mismatch")
	}
	return nil
}
