package operations

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const DependencySchemaVersion = "edu-agent.operations.dependencies/v1"

var noteSyncAuthorityPattern = regexp.MustCompile("The supported contract is Fast Note Sync Service `([^`]+)` at commit `([0-9a-f]{40})` with Obsidian plugin `([^`]+)` at commit `([0-9a-f]{40})`")

type DependencyLock struct {
	SchemaVersion               string `json:"schema_version"`
	GoVersion                   string `json:"go_version"`
	NoteSyncAuthoritySpecSHA256 string `json:"-"`
	Postgres                    struct {
		Image    string `json:"image"`
		Platform string `json:"platform"`
	} `json:"postgres"`
	NoteSync struct {
		ServiceImage   string `json:"service_image"`
		ServiceVersion string `json:"service_version"`
		ServiceCommit  string `json:"service_commit"`
		PluginVersion  string `json:"plugin_version"`
		PluginCommit   string `json:"plugin_commit"`
		Platform       string `json:"platform"`
	} `json:"notesync"`
	Nocturne struct {
		Platform               string `json:"platform"`
		PlatformManifestDigest string `json:"platform_manifest_digest"`
		ConfigDigest           string `json:"config_digest"`
		ImageLockSHA256        string `json:"image_lock_sha256"`
		SupplyChainLockSHA256  string `json:"supply_chain_lock_sha256"`
	} `json:"nocturne"`
}

type nocturneImageLock struct {
	Schema                 string  `json:"schema"`
	SupplyChainLockSHA256  string  `json:"supply_chain_lock_sha256"`
	Platform               string  `json:"platform"`
	OCIIndexSHA256         string  `json:"oci_index_sha256"`
	PlatformManifestDigest string  `json:"platform_manifest_digest"`
	ConfigDigest           string  `json:"config_digest"`
	RegistryDigest         *string `json:"registry_digest"`
}

func LoadDependencyLock(root string) (DependencyLock, string, error) {
	path := filepath.Join(root, "contracttests", "operations", "dependencies.json")
	content, err := os.ReadFile(path)
	if err != nil {
		return DependencyLock{}, "", err
	}
	var lock DependencyLock
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lock); err != nil {
		return DependencyLock{}, "", fmt.Errorf("decode dependency lock: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return DependencyLock{}, "", errors.New("dependency lock contains trailing JSON")
		}
		return DependencyLock{}, "", fmt.Errorf("decode dependency lock trailing data: %w", err)
	}
	if err := validateDependencyLock(lock); err != nil {
		return DependencyLock{}, "", err
	}
	authorityDigest, err := validateNoteSyncAuthority(root, lock)
	if err != nil {
		return DependencyLock{}, "", err
	}
	lock.NoteSyncAuthoritySpecSHA256 = authorityDigest
	digest, _, err := HashFile(path)
	if err != nil {
		return DependencyLock{}, "", err
	}
	if err := validateNocturneLocks(root, lock); err != nil {
		return DependencyLock{}, "", err
	}
	return lock, digest, nil
}

func validateDependencyLock(lock DependencyLock) error {
	if lock.SchemaVersion != DependencySchemaVersion {
		return fmt.Errorf("unsupported dependency schema %q", lock.SchemaVersion)
	}
	if !strings.HasPrefix(lock.GoVersion, "go1.") {
		return errors.New("dependency lock Go version is invalid")
	}
	if !strings.Contains(lock.Postgres.Image, "@sha256:") || lock.Postgres.Platform == "" {
		return errors.New("PostgreSQL image and platform must be digest pinned")
	}
	if !strings.Contains(lock.NoteSync.ServiceImage, "@sha256:") || lock.NoteSync.ServiceVersion == "" || len(lock.NoteSync.ServiceCommit) != 40 || lock.NoteSync.PluginVersion == "" || len(lock.NoteSync.PluginCommit) != 40 || lock.NoteSync.Platform == "" {
		return errors.New("Fast Note Sync service/plugin lock is incomplete")
	}
	if lock.Nocturne.Platform == "" || !strings.HasPrefix(lock.Nocturne.PlatformManifestDigest, "sha256:") || !strings.HasPrefix(lock.Nocturne.ConfigDigest, "sha256:") || len(lock.Nocturne.ImageLockSHA256) != 64 || len(lock.Nocturne.SupplyChainLockSHA256) != 64 {
		return errors.New("Nocturne dependency lock is incomplete")
	}
	return nil
}

func validateNoteSyncAuthority(root string, lock DependencyLock) (string, error) {
	path := filepath.Join(root, "docs", "comet", "specs", "notesync-bridge", "spec.md")
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read Fast Note Sync authority spec: %w", err)
	}
	if err := validateNoteSyncAuthorityDocument(content, lock); err != nil {
		return "", err
	}
	digest, _, err := HashFile(path)
	if err != nil {
		return "", err
	}
	return digest, nil
}

func validateNoteSyncAuthorityDocument(content []byte, lock DependencyLock) error {
	matches := noteSyncAuthorityPattern.FindAllSubmatch(content, -1)
	if len(matches) != 1 {
		return errors.New("Fast Note Sync authority spec must contain exactly one promoted service/plugin contract")
	}
	promoted := matches[0]
	if string(promoted[1]) != lock.NoteSync.ServiceVersion || string(promoted[2]) != lock.NoteSync.ServiceCommit || string(promoted[3]) != lock.NoteSync.PluginVersion || string(promoted[4]) != lock.NoteSync.PluginCommit {
		return errors.New("Fast Note Sync dependency index differs from the promoted authority spec")
	}
	return nil
}

func validateNocturneLocks(root string, lock DependencyLock) error {
	imageLockPath := filepath.Join(root, "deploy", "nocturne", "image.lock.json")
	imageDigest, _, err := HashFile(imageLockPath)
	if err != nil {
		return err
	}
	if imageDigest != lock.Nocturne.ImageLockSHA256 {
		return errors.New("Nocturne image lock digest differs from the approved dependency index")
	}
	supplyPath := filepath.Join(root, "deploy", "nocturne", "supply-chain.lock.json")
	supplyDigest, _, err := HashFile(supplyPath)
	if err != nil {
		return err
	}
	if supplyDigest != lock.Nocturne.SupplyChainLockSHA256 {
		return errors.New("Nocturne supply-chain lock digest differs from the approved dependency index")
	}
	var imageLock nocturneImageLock
	if err := readJSONStrict(imageLockPath, &imageLock); err != nil {
		return err
	}
	if imageLock.SupplyChainLockSHA256 != supplyDigest || imageLock.Platform != lock.Nocturne.Platform || imageLock.PlatformManifestDigest != lock.Nocturne.PlatformManifestDigest || imageLock.ConfigDigest != lock.Nocturne.ConfigDigest {
		return errors.New("Nocturne image lock values differ from the approved dependency index")
	}
	return nil
}
