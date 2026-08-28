package operations

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testAttestor(t *testing.T) *Attestor {
	t.Helper()
	attestor, err := NewAttestor(bytes.Repeat([]byte{0x42}, attestationKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	return attestor
}

func testMaterial() KeyMaterial {
	return KeyMaterial{
		CandidateFingerprint: strings.Repeat("a", 64),
		Lane:                 "model-vertical",
		Scenario:             "production-fake-model-postgresql",
		Command:              CommandRecord{Argv: []string{"go", "test", "-json", "./blackbox"}, Cwd: "/candidate"},
		Platform:             PlatformRecord{OS: "linux", Arch: "amd64", Runtime: "go1.26.6", Kernel: "Linux test"},
		Toolchain:            map[string]string{"go": "go version go1.26.6 linux/amd64"},
		PinnedInputs:         map[string]string{"runner_sha256": strings.Repeat("b", 64), "postgres_image": "postgres@sha256:" + strings.Repeat("c", 64)},
		SelectedTests:        []string{"TestBlackBoxProductionFakeModelVerticalPostgreSQL"},
		ExpectedGoTests:      []string{"TestBlackBoxProductionFakeModelVerticalPostgreSQL"},
	}
}

func TestEvidenceKeyInvalidation(t *testing.T) {
	baseline := testMaterial()
	baselineKey, err := ComputeEvidenceKey(baseline)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*KeyMaterial){
		"candidate": func(value *KeyMaterial) { value.CandidateFingerprint = strings.Repeat("d", 64) },
		"lane":      func(value *KeyMaterial) { value.Lane = "offline-blackbox" },
		"argv":      func(value *KeyMaterial) { value.Command.Argv = append(value.Command.Argv, "-count=1") },
		"cwd":       func(value *KeyMaterial) { value.Command.Cwd = "/other" },
		"platform":  func(value *KeyMaterial) { value.Platform.Arch = "arm64" },
		"toolchain": func(value *KeyMaterial) { value.Toolchain["go"] = "go version go1.27.0 linux/amd64" },
		"pin":       func(value *KeyMaterial) { value.PinnedInputs["runner_sha256"] = strings.Repeat("e", 64) },
		"selection": func(value *KeyMaterial) {
			value.SelectedTests = []string{"TestDifferent"}
			value.ExpectedGoTests = []string{"TestDifferent"}
		},
		"expected": func(value *KeyMaterial) {
			value.ExpectedGoTests = []string{"TestDifferent"}
			value.SelectedTests = append(value.SelectedTests, "TestDifferent")
		},
		"any-go": func(value *KeyMaterial) { value.RequireAnyGoTest = true },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			value := testMaterial()
			mutate(&value)
			key, err := ComputeEvidenceKey(value)
			if err != nil {
				t.Fatal(err)
			}
			if key == baselineKey {
				t.Fatalf("%s change did not invalidate evidence key", name)
			}
		})
	}
}

func TestPassedEvidenceReuseAndCorruptionRejection(t *testing.T) {
	root := t.TempDir()
	attestor := testAttestor(t)
	material := testMaterial()
	key, err := ComputeEvidenceKey(material)
	if err != nil {
		t.Fatal(err)
	}
	logRelative := filepath.ToSlash(filepath.Join("lanes", "model-vertical", key+".log"))
	logPath := filepath.Join(root, filepath.FromSlash(logRelative))
	logContent := []byte(strings.Join([]string{
		`{"Action":"run","Package":"example/blackbox","Test":"TestBlackBoxProductionFakeModelVerticalPostgreSQL"}`,
		`{"Action":"pass","Package":"example/blackbox","Test":"TestBlackBoxProductionFakeModelVerticalPostgreSQL"}`,
	}, "\n") + "\n")
	if err := AtomicWriteFile(logPath, logContent, 0o600); err != nil {
		t.Fatal(err)
	}
	logDigest, logBytes, err := HashFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	now := time.Now().UTC()
	evidence := Evidence{
		SchemaVersion:        EvidenceSchemaVersion,
		EvidenceKey:          key,
		Attempt:              1,
		CandidateFingerprint: material.CandidateFingerprint,
		Lane:                 material.Lane,
		Scenario:             material.Scenario,
		Command:              material.Command,
		Status:               StatusPassed,
		Reason:               "required target passed",
		Exit:                 ExitRecord{Started: true, Code: &zero},
		StartedAt:            now.Format(time.RFC3339Nano),
		FinishedAt:           now.Add(time.Second).Format(time.RFC3339Nano),
		Platform:             material.Platform,
		Toolchain:            material.Toolchain,
		PinnedInputs:         material.PinnedInputs,
		Tests: TestRecord{
			Selected:   material.SelectedTests,
			ExpectedGo: material.ExpectedGoTests,
			Executed:   material.SelectedTests,
			Passed:     material.SelectedTests,
		},
		Log: LogRecord{Path: logRelative, SHA256: logDigest, Bytes: logBytes},
	}
	manifestPath := filepath.Join(root, "lanes", "model-vertical", key+".json")
	if err := AtomicWriteJSON(manifestPath, evidence, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyPassedEvidence(manifestPath, root, material, attestor); err == nil {
		t.Fatal("hand-written public-hash evidence without an attestation was accepted")
	}
	if err := attestor.SignEvidence(&evidence); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteJSON(manifestPath, evidence, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyPassedEvidence(manifestPath, root, material, attestor); err != nil {
		t.Fatalf("matching passed evidence was not reusable: %v", err)
	}
	if err := os.WriteFile(logPath, []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyPassedEvidence(manifestPath, root, material, attestor); err == nil {
		t.Fatal("tampered log was accepted")
	}
	if err := AtomicWriteFile(logPath, []byte("hand-edited but internally rehashed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	evidence.Log.SHA256, evidence.Log.Bytes, err = HashFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteJSON(manifestPath, evidence, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyPassedEvidence(manifestPath, root, material, attestor); err == nil {
		t.Fatal("synchronized manifest and log edit without target events was accepted")
	}
	if err := AtomicWriteFile(logPath, logContent, 0o600); err != nil {
		t.Fatal(err)
	}
	evidence.Log.SHA256, evidence.Log.Bytes, err = HashFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteJSON(manifestPath, evidence, 0o600); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := strings.Replace(string(content), "\n}", ",\n  \"unknown_field\": true\n}", 1)
	if err := os.WriteFile(manifestPath, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyPassedEvidence(manifestPath, root, material, attestor); err == nil {
		t.Fatal("manifest with an unknown field was accepted")
	}
}

func TestIndexAttestationAndEvidenceKeyBinding(t *testing.T) {
	attestor := testAttestor(t)
	candidate := strings.Repeat("c", 64)
	manifestKey := strings.Repeat("a", 64)
	lanes := []IndexLane{{Lane: "a", Scenario: "one", Status: StatusPassed, Reason: "passed", EvidenceKey: manifestKey, EvidencePath: "lanes/a.json"}}
	qualificationKey, err := ComputeQualificationKey(candidate, lanes)
	if err != nil {
		t.Fatal(err)
	}
	index := CandidateIndex{
		SchemaVersion:        IndexSchemaVersion,
		CandidateFingerprint: candidate,
		GeneratedAt:          time.Now().UTC().Format(time.RFC3339Nano),
		Overall:              StatusPassed,
		QualificationKey:     qualificationKey,
		Lanes:                lanes,
	}
	if err := attestor.SignCandidateIndex(&index); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "candidate-index.json")
	if err := AtomicWriteJSON(path, index, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCandidateIndexStrict(path, attestor); err != nil {
		t.Fatalf("signed candidate index was rejected: %v", err)
	}
	index.Lanes[0].EvidenceKey = strings.Repeat("b", 64)
	index.QualificationKey, err = ComputeQualificationKey(candidate, index.Lanes)
	if err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteJSON(path, index, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCandidateIndexStrict(path, attestor); err == nil {
		t.Fatal("candidate index key tamper without a new attestation was accepted")
	}

	evidence := Evidence{EvidenceKey: manifestKey}
	entry := IndexLane{EvidenceKey: manifestKey}
	if err := ValidateIndexEvidenceBinding(entry, evidence, manifestKey); err != nil {
		t.Fatal(err)
	}
	entry.EvidenceKey = strings.Repeat("b", 64)
	if err := ValidateIndexEvidenceBinding(entry, evidence, manifestKey); err == nil {
		t.Fatal("index key differing from current and manifest keys was accepted")
	}
}

func TestAtomicWriteFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "nested", "evidence.json")
	if err := AtomicWriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "second" {
		t.Fatalf("atomic replacement content=%q", content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("atomic file mode=%o want=600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("atomic writer left temporary files: %v", entries)
	}
}

func TestStatusAggregationAndIndexKey(t *testing.T) {
	tests := []struct {
		statuses []Status
		want     Status
	}{
		{[]Status{StatusPassed, StatusReused}, StatusPassed},
		{[]Status{StatusPassed, StatusNotRun}, StatusNotRun},
		{[]Status{StatusPassed, StatusBlocked, StatusNotRun}, StatusBlocked},
		{[]Status{StatusBlocked, StatusFailed}, StatusFailed},
		{nil, StatusNotRun},
	}
	for _, test := range tests {
		if got := AggregateStatuses(test.statuses); got != test.want {
			t.Fatalf("AggregateStatuses(%v)=%s want=%s", test.statuses, got, test.want)
		}
	}
	lanes := []IndexLane{
		{Lane: "b", Scenario: "two", Status: StatusReused, EvidenceKey: strings.Repeat("b", 64)},
		{Lane: "a", Scenario: "one", Status: StatusPassed, EvidenceKey: strings.Repeat("a", 64)},
	}
	first, err := ComputeQualificationKey(strings.Repeat("c", 64), lanes)
	if err != nil {
		t.Fatal(err)
	}
	reversed := []IndexLane{lanes[1], lanes[0]}
	second, err := ComputeQualificationKey(strings.Repeat("c", 64), reversed)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("qualification key depends on lane order")
	}
}
