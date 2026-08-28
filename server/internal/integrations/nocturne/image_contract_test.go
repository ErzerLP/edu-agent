package nocturne

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/memory"
)

func TestImageContractMatchesDeploymentLocks(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate image contract test")
	}
	root := filepath.Join(filepath.Dir(filename), "..", "..", "..", "..")
	imagePayload, err := os.ReadFile(filepath.Join(root, "deploy", "nocturne", "image.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var imageLock struct {
		SupplyChainLockSHA256  string  `json:"supply_chain_lock_sha256"`
		Platform               string  `json:"platform"`
		PlatformManifestDigest string  `json:"platform_manifest_digest"`
		ConfigDigest           string  `json:"config_digest"`
		RegistryDigest         *string `json:"registry_digest"`
	}
	if err := json.Unmarshal(imagePayload, &imageLock); err != nil {
		t.Fatal(err)
	}
	if imageLock.Platform != ImagePlatform || imageLock.PlatformManifestDigest != ImagePlatformManifestDigest ||
		imageLock.ConfigDigest != ImageConfigDigest || imageLock.SupplyChainLockSHA256 != ImageSupplyChainLockSHA256 ||
		imageLock.RegistryDigest != nil {
		t.Fatalf("Go image contract does not match image.lock.json: %+v", imageLock)
	}

	inputPayload, err := os.ReadFile(filepath.Join(root, "deploy", "nocturne", "supply-chain.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inputLock struct {
		Upstream struct {
			Commit string `json:"commit"`
		} `json:"upstream"`
		Overlay struct {
			Revision string `json:"revision"`
		} `json:"overlay"`
	}
	if err := json.Unmarshal(inputPayload, &inputLock); err != nil {
		t.Fatal(err)
	}
	if inputLock.Upstream.Commit != ImageUpstreamCommit || inputLock.Overlay.Revision != ImageCompatibilityRevision ||
		memory.NocturneUpstreamCommit != ImageUpstreamCommit || memory.NocturneCompatRevision != ImageCompatibilityRevision {
		t.Fatalf("Go capability contract does not match supply-chain.lock.json: %+v", inputLock)
	}
}
