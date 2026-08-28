package operations

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

const (
	EvidenceSchemaVersion = "edu-agent.operations.evidence/v3"
	IndexSchemaVersion    = "edu-agent.operations.candidate-index/v2"
)

type Status string

const (
	StatusPassed  Status = "passed"
	StatusFailed  Status = "failed"
	StatusBlocked Status = "blocked"
	StatusNotRun  Status = "not-run"
	StatusReused  Status = "reused"
)

type CommandRecord struct {
	Argv []string `json:"argv"`
	Cwd  string   `json:"cwd"`
}

type PlatformRecord struct {
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Runtime string `json:"runtime"`
	Kernel  string `json:"kernel"`
}

type ExitRecord struct {
	Started bool `json:"started"`
	Code    *int `json:"code"`
}

type TestRecord struct {
	Selected         []string `json:"selected"`
	ExpectedGo       []string `json:"expected_go"`
	ExternalTargets  []string `json:"external_targets"`
	OutputAssertions []string `json:"output_assertions"`
	RequireAnyGoTest bool     `json:"require_any_go_test"`
	Executed         []string `json:"executed"`
	Passed           []string `json:"passed"`
	Failed           []string `json:"failed"`
	Skipped          []string `json:"skipped"`
}

type LogRecord struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type KeyMaterial struct {
	CandidateFingerprint string            `json:"candidate_fingerprint"`
	Lane                 string            `json:"lane"`
	Scenario             string            `json:"scenario"`
	Command              CommandRecord     `json:"command"`
	Platform             PlatformRecord    `json:"platform"`
	Toolchain            map[string]string `json:"toolchain"`
	PinnedInputs         map[string]string `json:"pinned_upstream_inputs"`
	SelectedTests        []string          `json:"selected_tests"`
	ExpectedGoTests      []string          `json:"expected_go_tests"`
	ExternalTargets      []string          `json:"external_targets"`
	OutputAssertions     []string          `json:"output_assertions"`
	RequireAnyGoTest     bool              `json:"require_any_go_test"`
}

type Evidence struct {
	SchemaVersion        string            `json:"schema_version"`
	EvidenceKey          string            `json:"evidence_key"`
	Attempt              int               `json:"attempt"`
	CandidateFingerprint string            `json:"candidate_fingerprint"`
	Lane                 string            `json:"lane"`
	Scenario             string            `json:"scenario"`
	Command              CommandRecord     `json:"command"`
	Status               Status            `json:"status"`
	Reason               string            `json:"reason"`
	Exit                 ExitRecord        `json:"exit"`
	StartedAt            string            `json:"started_at"`
	FinishedAt           string            `json:"finished_at"`
	Platform             PlatformRecord    `json:"platform"`
	Toolchain            map[string]string `json:"toolchain"`
	PinnedInputs         map[string]string `json:"pinned_upstream_inputs"`
	Tests                TestRecord        `json:"tests"`
	Log                  LogRecord         `json:"log"`
	Attestation          Attestation       `json:"attestation"`
}

type IndexLane struct {
	Lane         string `json:"lane"`
	Scenario     string `json:"scenario"`
	Status       Status `json:"status"`
	Reason       string `json:"reason"`
	EvidenceKey  string `json:"evidence_key"`
	EvidencePath string `json:"evidence_path"`
}

type CandidateIndex struct {
	SchemaVersion        string      `json:"schema_version"`
	CandidateFingerprint string      `json:"candidate_fingerprint"`
	GeneratedAt          string      `json:"generated_at"`
	Overall              Status      `json:"overall"`
	QualificationKey     string      `json:"qualification_key"`
	Lanes                []IndexLane `json:"lanes"`
	Attestation          Attestation `json:"attestation"`
}

func (attestor *Attestor) SignEvidence(evidence *Evidence) error {
	if attestor == nil {
		return errors.New("attestor is required")
	}
	evidence.Attestation = Attestation{Algorithm: AttestationAlgorithm, KeyID: attestor.keyID}
	signature, err := attestor.signature(evidenceDomain, *evidence)
	if err != nil {
		return err
	}
	evidence.Attestation.Signature = signature
	return nil
}

func (attestor *Attestor) VerifyEvidence(evidence Evidence) error {
	return attestor.verify(evidenceDomain, evidence.Attestation, func() (any, error) {
		evidence.Attestation.Signature = ""
		return evidence, nil
	})
}

func (attestor *Attestor) SignCandidateIndex(index *CandidateIndex) error {
	if attestor == nil {
		return errors.New("attestor is required")
	}
	index.Attestation = Attestation{Algorithm: AttestationAlgorithm, KeyID: attestor.keyID}
	signature, err := attestor.signature(indexDomain, *index)
	if err != nil {
		return err
	}
	index.Attestation.Signature = signature
	return nil
}

func (attestor *Attestor) VerifyCandidateIndex(index CandidateIndex) error {
	return attestor.verify(indexDomain, index.Attestation, func() (any, error) {
		index.Attestation.Signature = ""
		return index, nil
	})
}

func ComputeEvidenceKey(material KeyMaterial) (string, error) {
	material = normalizeKeyMaterial(material)
	if err := validateKeyMaterial(material); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(material)
	if err != nil {
		return "", fmt.Errorf("marshal evidence key material: %w", err)
	}
	sum := sha256.Sum256(append([]byte(EvidenceSchemaVersion+"\x00"), encoded...))
	return hex.EncodeToString(sum[:]), nil
}

func (e Evidence) KeyMaterial() KeyMaterial {
	return normalizeKeyMaterial(KeyMaterial{
		CandidateFingerprint: e.CandidateFingerprint,
		Lane:                 e.Lane,
		Scenario:             e.Scenario,
		Command:              e.Command,
		Platform:             e.Platform,
		Toolchain:            e.Toolchain,
		PinnedInputs:         e.PinnedInputs,
		SelectedTests:        e.Tests.Selected,
		ExpectedGoTests:      e.Tests.ExpectedGo,
		ExternalTargets:      e.Tests.ExternalTargets,
		OutputAssertions:     e.Tests.OutputAssertions,
		RequireAnyGoTest:     e.Tests.RequireAnyGoTest,
	})
}

func normalizeKeyMaterial(material KeyMaterial) KeyMaterial {
	material.Command.Argv = append([]string(nil), material.Command.Argv...)
	material.Toolchain = cloneMap(material.Toolchain)
	material.PinnedInputs = cloneMap(material.PinnedInputs)
	material.SelectedTests = sortedUnique(material.SelectedTests)
	material.ExpectedGoTests = sortedUnique(material.ExpectedGoTests)
	material.ExternalTargets = sortedUnique(material.ExternalTargets)
	material.OutputAssertions = sortedUnique(material.OutputAssertions)
	return material
}

func cloneMap(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func sortedUnique(input []string) []string {
	seen := make(map[string]struct{}, len(input))
	result := make([]string, 0, len(input))
	for _, value := range input {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func validateKeyMaterial(material KeyMaterial) error {
	if len(material.CandidateFingerprint) != 64 || !isLowerHex(material.CandidateFingerprint) {
		return errors.New("candidate fingerprint must be a lowercase SHA-256")
	}
	if strings.TrimSpace(material.Lane) == "" || strings.TrimSpace(material.Scenario) == "" {
		return errors.New("lane and scenario are required")
	}
	if len(material.Command.Argv) == 0 || strings.TrimSpace(material.Command.Cwd) == "" {
		return errors.New("command argv and cwd are required")
	}
	for _, value := range material.Command.Argv {
		if strings.TrimSpace(value) == "" {
			return errors.New("command argv contains an empty value")
		}
	}
	if material.Platform.OS == "" || material.Platform.Arch == "" || material.Platform.Runtime == "" || material.Platform.Kernel == "" {
		return errors.New("complete platform identity is required")
	}
	if len(material.Toolchain) == 0 || len(material.PinnedInputs) == 0 {
		return errors.New("toolchain and pinned upstream inputs are required")
	}
	for key, value := range material.Toolchain {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			return errors.New("toolchain contains an empty key or value")
		}
	}
	for key, value := range material.PinnedInputs {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			return errors.New("pinned upstream inputs contain an empty key or value")
		}
	}
	if len(material.SelectedTests) == 0 {
		return errors.New("selected tests or scenario identifiers are required")
	}
	for _, target := range append(append([]string(nil), material.ExpectedGoTests...), material.ExternalTargets...) {
		if !containsString(material.SelectedTests, target) {
			return fmt.Errorf("required target %q is not present in selected tests", target)
		}
	}
	if len(material.ExternalTargets) > 0 && len(material.OutputAssertions) == 0 {
		return errors.New("external targets require output assertions")
	}
	if len(material.ExpectedGoTests) == 0 && !material.RequireAnyGoTest && len(material.ExternalTargets) == 0 {
		return errors.New("key material has no executable coverage rule")
	}
	return nil
}

func ValidateEvidence(e Evidence) error {
	if e.SchemaVersion != EvidenceSchemaVersion {
		return fmt.Errorf("unsupported evidence schema %q", e.SchemaVersion)
	}
	if !validStatus(e.Status) {
		return fmt.Errorf("invalid evidence status %q", e.Status)
	}
	if strings.TrimSpace(e.Reason) == "" {
		return errors.New("evidence reason is required")
	}
	material := e.KeyMaterial()
	if err := validateKeyMaterial(material); err != nil {
		return err
	}
	key, err := ComputeEvidenceKey(material)
	if err != nil {
		return err
	}
	if e.EvidenceKey != key {
		return errors.New("evidence key does not match its key material")
	}
	started, err := time.Parse(time.RFC3339Nano, e.StartedAt)
	if err != nil {
		return errors.New("started_at must be RFC3339Nano")
	}
	finished, err := time.Parse(time.RFC3339Nano, e.FinishedAt)
	if err != nil {
		return errors.New("finished_at must be RFC3339Nano")
	}
	if finished.Before(started) {
		return errors.New("finished_at precedes started_at")
	}
	if e.Exit.Started && e.Exit.Code == nil {
		return errors.New("started command requires an exit code")
	}
	if !e.Exit.Started && e.Exit.Code != nil {
		return errors.New("non-started command cannot have an exit code")
	}
	if e.Attempt < 1 {
		return errors.New("evidence attempt must be at least one")
	}
	if e.Status == StatusPassed {
		if !e.Exit.Started || e.Exit.Code == nil || *e.Exit.Code != 0 {
			return errors.New("passed evidence requires a started command with exit code zero")
		}
		if len(e.Tests.Executed) == 0 || len(e.Tests.Passed) == 0 {
			return errors.New("passed evidence requires executed and passed targets")
		}
		if len(e.Tests.Failed) > 0 || len(e.Tests.Skipped) > 0 {
			return errors.New("passed evidence cannot contain failed or skipped required targets")
		}
		for _, target := range append(append([]string(nil), e.Tests.ExpectedGo...), e.Tests.ExternalTargets...) {
			if !containsString(e.Tests.Executed, target) || !containsString(e.Tests.Passed, target) {
				return fmt.Errorf("passed evidence does not prove required target %q", target)
			}
		}
	}
	if strings.TrimSpace(e.Log.Path) == "" || len(e.Log.SHA256) != 64 || !isLowerHex(e.Log.SHA256) || e.Log.Bytes < 0 {
		return errors.New("complete log metadata is required")
	}
	if err := validateAttestationMetadata(e.Attestation); err != nil {
		return err
	}
	return nil
}

func ReadEvidenceStrict(path string, attestor *Attestor) (Evidence, error) {
	var evidence Evidence
	if err := readJSONStrict(path, &evidence); err != nil {
		return Evidence{}, err
	}
	if err := ValidateEvidence(evidence); err != nil {
		return Evidence{}, err
	}
	if err := attestor.VerifyEvidence(evidence); err != nil {
		return Evidence{}, err
	}
	return evidence, nil
}

func VerifyPassedEvidence(path, evidenceRoot string, expected KeyMaterial, attestor *Attestor) (Evidence, error) {
	expected = normalizeKeyMaterial(expected)
	expectedKey, err := ComputeEvidenceKey(expected)
	if err != nil {
		return Evidence{}, err
	}
	evidence, err := ReadEvidenceStrict(path, attestor)
	if err != nil {
		return Evidence{}, err
	}
	if evidence.Status != StatusPassed {
		return Evidence{}, fmt.Errorf("evidence status %q is not reusable", evidence.Status)
	}
	if evidence.EvidenceKey != expectedKey || !reflect.DeepEqual(evidence.KeyMaterial(), expected) {
		return Evidence{}, errors.New("evidence inputs do not match the current candidate key")
	}
	logPath, err := safeEvidencePath(evidenceRoot, evidence.Log.Path)
	if err != nil {
		return Evidence{}, err
	}
	digest, size, err := HashFile(logPath)
	if err != nil {
		return Evidence{}, fmt.Errorf("verify evidence log: %w", err)
	}
	if digest != evidence.Log.SHA256 || size != evidence.Log.Bytes {
		return Evidence{}, errors.New("evidence log digest or byte count mismatch")
	}
	content, err := os.ReadFile(logPath)
	if err != nil {
		return Evidence{}, fmt.Errorf("read evidence log: %w", err)
	}
	coverage, err := AnalyzeLogCoverage(content, expected)
	if err != nil {
		return Evidence{}, fmt.Errorf("reparse evidence log: %w", err)
	}
	if !reflect.DeepEqual(normalizeTestRecord(evidence.Tests), normalizeTestRecord(coverage)) {
		return Evidence{}, errors.New("evidence test coverage does not match its durable log")
	}
	return evidence, nil
}

func AnalyzeLogCoverage(content []byte, material KeyMaterial) (TestRecord, error) {
	material = normalizeKeyMaterial(material)
	if err := validateKeyMaterial(material); err != nil {
		return TestRecord{}, err
	}
	var summary GoEventSummary
	var err error
	switch {
	case len(material.ExpectedGoTests) > 0:
		summary, err = ParseExpectedGoTestEvents(bytes.NewReader(content), material.ExpectedGoTests)
	case material.RequireAnyGoTest:
		summary, err = ParseAnyGoTestEvents(bytes.NewReader(content))
	}
	if err != nil {
		return TestRecord{}, err
	}
	for _, assertion := range material.OutputAssertions {
		if !bytes.Contains(content, []byte(assertion)) {
			return TestRecord{}, fmt.Errorf("runner output did not prove pinned input: %s", assertion)
		}
	}
	record := TestRecord{
		Selected:         material.SelectedTests,
		ExpectedGo:       material.ExpectedGoTests,
		ExternalTargets:  material.ExternalTargets,
		OutputAssertions: material.OutputAssertions,
		RequireAnyGoTest: material.RequireAnyGoTest,
		Executed:         sortedUnique(append(summary.Executed, material.ExternalTargets...)),
		Passed:           sortedUnique(append(summary.Passed, material.ExternalTargets...)),
		Failed:           sortedUnique(summary.Failed),
		Skipped:          sortedUnique(summary.Skipped),
	}
	if len(record.Executed) == 0 || len(record.Passed) == 0 {
		return TestRecord{}, errors.New("lane log contains no executed passing scenario")
	}
	if len(record.Failed) > 0 || len(record.Skipped) > 0 {
		return TestRecord{}, errors.New("lane log contains failed or skipped required targets")
	}
	return normalizeTestRecord(record), nil
}

func normalizeTestRecord(record TestRecord) TestRecord {
	record.Selected = sortedUnique(record.Selected)
	record.ExpectedGo = sortedUnique(record.ExpectedGo)
	record.ExternalTargets = sortedUnique(record.ExternalTargets)
	record.OutputAssertions = sortedUnique(record.OutputAssertions)
	record.Executed = sortedUnique(record.Executed)
	record.Passed = sortedUnique(record.Passed)
	record.Failed = sortedUnique(record.Failed)
	record.Skipped = sortedUnique(record.Skipped)
	return record
}

func safeEvidencePath(root, relative string) (string, error) {
	if filepath.IsAbs(relative) || relative == "." || relative == "" {
		return "", errors.New("evidence log path must be relative")
	}
	clean := filepath.Clean(relative)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("evidence log path escapes the evidence directory")
	}
	joined := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("evidence log path escapes the evidence directory")
	}
	return joined, nil
}

func AggregateStatuses(statuses []Status) Status {
	if len(statuses) == 0 {
		return StatusNotRun
	}
	hasBlocked := false
	hasNotRun := false
	for _, status := range statuses {
		switch status {
		case StatusFailed:
			return StatusFailed
		case StatusBlocked:
			hasBlocked = true
		case StatusNotRun:
			hasNotRun = true
		case StatusPassed, StatusReused:
		default:
			return StatusFailed
		}
	}
	if hasBlocked {
		return StatusBlocked
	}
	if hasNotRun {
		return StatusNotRun
	}
	return StatusPassed
}

func ComputeQualificationKey(candidate string, lanes []IndexLane) (string, error) {
	if len(candidate) != 64 || !isLowerHex(candidate) {
		return "", errors.New("candidate fingerprint must be a lowercase SHA-256")
	}
	type qualificationLane struct {
		Lane        string `json:"lane"`
		Scenario    string `json:"scenario"`
		Status      Status `json:"status"`
		EvidenceKey string `json:"evidence_key"`
	}
	values := make([]qualificationLane, 0, len(lanes))
	seen := make(map[string]struct{}, len(lanes))
	for _, lane := range lanes {
		if lane.Lane == "" || lane.Scenario == "" || !validStatus(lane.Status) {
			return "", errors.New("candidate index contains an invalid lane")
		}
		if _, ok := seen[lane.Lane]; ok {
			return "", fmt.Errorf("candidate index repeats lane %q", lane.Lane)
		}
		seen[lane.Lane] = struct{}{}
		values = append(values, qualificationLane{Lane: lane.Lane, Scenario: lane.Scenario, Status: lane.Status, EvidenceKey: lane.EvidenceKey})
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Lane < values[j].Lane })
	encoded, err := json.Marshal(struct {
		Schema    string              `json:"schema_version"`
		Candidate string              `json:"candidate_fingerprint"`
		Lanes     []qualificationLane `json:"lanes"`
	}{IndexSchemaVersion, candidate, values})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func ValidateIndexEvidenceBinding(entry IndexLane, evidence Evidence, expectedKey string) error {
	if entry.EvidenceKey == "" || expectedKey == "" {
		return errors.New("candidate index evidence key binding is incomplete")
	}
	if entry.EvidenceKey != expectedKey {
		return errors.New("candidate index evidence key does not match current inputs")
	}
	if evidence.EvidenceKey != entry.EvidenceKey {
		return errors.New("candidate index evidence key differs from its manifest")
	}
	return nil
}

func ValidateCandidateIndex(index CandidateIndex) error {
	if index.SchemaVersion != IndexSchemaVersion {
		return fmt.Errorf("unsupported candidate index schema %q", index.SchemaVersion)
	}
	if _, err := time.Parse(time.RFC3339Nano, index.GeneratedAt); err != nil {
		return errors.New("candidate index generated_at must be RFC3339Nano")
	}
	statuses := make([]Status, 0, len(index.Lanes))
	for _, lane := range index.Lanes {
		statuses = append(statuses, lane.Status)
		if strings.TrimSpace(lane.Reason) == "" {
			return fmt.Errorf("candidate index lane %q has no reason", lane.Lane)
		}
		if (lane.Status == StatusPassed || lane.Status == StatusReused) && (lane.EvidenceKey == "" || lane.EvidencePath == "") {
			return fmt.Errorf("candidate index lane %q lacks reusable evidence", lane.Lane)
		}
	}
	if AggregateStatuses(statuses) != index.Overall {
		return errors.New("candidate index overall status does not match lane aggregation")
	}
	key, err := ComputeQualificationKey(index.CandidateFingerprint, index.Lanes)
	if err != nil {
		return err
	}
	if key != index.QualificationKey {
		return errors.New("candidate index qualification key mismatch")
	}
	if err := validateAttestationMetadata(index.Attestation); err != nil {
		return err
	}
	return nil
}

func ReadCandidateIndexStrict(path string, attestor *Attestor) (CandidateIndex, error) {
	var index CandidateIndex
	if err := readJSONStrict(path, &index); err != nil {
		return CandidateIndex{}, err
	}
	if err := ValidateCandidateIndex(index); err != nil {
		return CandidateIndex{}, err
	}
	if err := attestor.VerifyCandidateIndex(index); err != nil {
		return CandidateIndex{}, err
	}
	return index, nil
}

func validateAttestationMetadata(attestation Attestation) error {
	if attestation.Algorithm != AttestationAlgorithm || len(attestation.KeyID) != 64 || !isLowerHex(attestation.KeyID) || len(attestation.Signature) != 64 || !isLowerHex(attestation.Signature) {
		return errors.New("complete HMAC-SHA256 attestation metadata is required")
	}
	return nil
}

func readJSONStrict(path string, target any) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode %s: trailing JSON value", path)
		}
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func AtomicWriteJSON(path string, value any, mode os.FileMode) error {
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	content = append(content, '\n')
	return AtomicWriteFile(path, content, mode)
}

func AtomicWriteFile(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".atomic-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempName)
	}
	if err := temp.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	if _, err := temp.Write(content); err != nil {
		cleanup()
		return err
	}
	if err := temp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return err
	}
	if err := os.Rename(tempName, path); err != nil {
		_ = os.Remove(tempName)
		return err
	}
	directory, err := os.Open(dir)
	if err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func HashFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func validStatus(status Status) bool {
	switch status {
	case StatusPassed, StatusFailed, StatusBlocked, StatusNotRun, StatusReused:
		return true
	default:
		return false
	}
}

func isLowerHex(value string) bool {
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
