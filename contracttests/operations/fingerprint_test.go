package operations

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCandidateFingerprintInvalidatesOnRelevantInputChange(t *testing.T) {
	root := t.TempDir()
	serverFile := filepath.Join(root, "server", "internal", "example.go")
	if err := os.MkdirAll(filepath.Dir(serverFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(serverFile, []byte("package internal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := CandidateFingerprint(root, "candidate-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(serverFile, []byte("package internal\n// changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := CandidateFingerprint(root, "candidate-1")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("relevant source change did not invalidate candidate fingerprint")
	}
}

func TestCandidateInputSnapshotRejectsSourceDrift(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "server", "internal", "drift.go")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("package internal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	expected, err := CandidateFingerprint(root, "candidate-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("package internal\n// drifted during lane execution\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := CandidateFingerprint(root, "candidate-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCandidateFingerprintSnapshot(expected, current); err == nil {
		t.Fatal("source drift was accepted as the original candidate snapshot")
	}
}

func TestCandidateFingerprintInvalidatesOnFormalContractChange(t *testing.T) {
	root := t.TempDir()
	contract := filepath.Join(root, "docs", "comet", "changes", "operations-hardening", "brief.md")
	if err := os.MkdirAll(filepath.Dir(contract), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(contract, []byte("# Acceptance examples\nA1: first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := CandidateFingerprint(root, "candidate-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(contract, []byte("# Acceptance examples\nA1: corrected\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := CandidateFingerprint(root, "candidate-1")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("formal contract change did not invalidate candidate fingerprint")
	}
}

func TestCandidateFingerprintIgnoresMutableCometState(t *testing.T) {
	root := t.TempDir()
	contract := filepath.Join(root, "docs", "comet", "changes", "operations-hardening", "brief.md")
	state := filepath.Join(root, "docs", "comet", "changes", "operations-hardening", "comet-state.yaml")
	if err := os.MkdirAll(filepath.Dir(contract), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(contract, []byte("# Acceptance examples\nA1: stable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, []byte("phase: build\nstate_version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := CandidateFingerprint(root, "candidate-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, []byte("phase: verify\nstate_version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := CandidateFingerprint(root, "candidate-1")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("mutable Comet workflow state changed the candidate fingerprint")
	}
}

func TestCandidateFingerprintExcludesEvidenceDirectory(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "contracttests", "operations", "source.go")
	evidence := filepath.Join(root, "contracttests", "operations", "evidence")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("package operations\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(evidence, 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := CandidateFingerprint(root, "candidate-1", evidence)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidence, "candidate-index.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := CandidateFingerprint(root, "candidate-1", evidence)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("evidence output changed the candidate fingerprint")
	}
}
