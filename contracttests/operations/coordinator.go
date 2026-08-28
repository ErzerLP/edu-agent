package operations

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultHostLockPath = "/tmp/edu-agent-operations-candidate.lock"
	HostLockProtocol    = "inherited-fd-v1"
)

type RunOptions struct {
	Root               string
	EvidenceDir        string
	SelectedLanes      []string
	Resume             bool
	DryRun             bool
	NocturneOCILayout  string
	CandidateID        string
	HostLockPath       string
	AttestationKeyFile string
}

type VerifyOptions struct {
	Root               string
	EvidenceDir        string
	NocturneOCILayout  string
	CandidateID        string
	HostLockPath       string
	AttestationKeyFile string
}

type LaneDescription struct {
	Name        string
	Scenario    string
	Description string
}

type laneDefinition struct {
	LaneDescription
	Argv             []string
	Environment      map[string]string
	Tools            []string
	PinnedInputs     map[string]string
	SelectedTests    []string
	ExpectedGoTests  []string
	RequireAnyGoTest bool
	ExternalTargets  []string
	Selections       []goSelection
	OutputAssertions []string
}

type goSelection struct {
	Cwd      string
	Package  string
	Regex    string
	Expected []string
}

type laneExecution struct {
	Status   Status
	Reason   string
	Exit     ExitRecord
	Tests    TestRecord
	Started  time.Time
	Finished time.Time
}

type preparedLane struct {
	definition       laneDefinition
	material         KeyMaterial
	key              string
	relativeManifest string
	relativeLog      string
	manifestPath     string
	logPath          string
	reused           bool
	preflightStatus  Status
	preflightReason  string
}

func LaneCatalog() []LaneDescription {
	return []LaneDescription{
		{Name: "model-vertical", Scenario: "production-fake-model-postgresql", Description: "production model client/adapter/application vertical with real PostgreSQL"},
		{Name: "postgres-db-core", Scenario: "postgres-db-core", Description: "application, knowledge, identity, outbox, platform and migration PostgreSQL contracts"},
		{Name: "postgres-learning-core", Scenario: "postgres-learning-core", Description: "learning PostgreSQL core contracts"},
		{Name: "postgres-learning-offline", Scenario: "postgres-learning-offline", Description: "learning Offline PostgreSQL contracts"},
		{Name: "postgres-learning-fault", Scenario: "postgres-learning-fault", Description: "learning typed-record fault matrix"},
		{Name: "postgres-memory", Scenario: "postgres-memory", Description: "memory PostgreSQL contracts"},
		{Name: "postgres-privacy-core", Scenario: "postgres-privacy-core", Description: "privacy PostgreSQL core contracts"},
		{Name: "postgres-privacy-fault", Scenario: "postgres-privacy-fault", Description: "privacy scrub fault matrix"},
		{Name: "offline-blackbox", Scenario: "offline-cli-blackbox-postgresql", Description: "real CLI Offline prepare/learn/sync/status black box with PostgreSQL"},
		{Name: "notesync-real", Scenario: "fast-note-sync-3.6.1-real-candidate", Description: "pinned Fast Note Sync real-container compatibility gate"},
		{Name: "nocturne-compose", Scenario: "nocturne-verified-oci-compose-full", Description: "verified Nocturne OCI layout and full Compose/PostgreSQL gate"},
	}
}

func FindRepositoryRoot(start string) (string, error) {
	current, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(current, "contracttests", "operations", "dependencies.json")); err == nil {
			if _, scriptErr := os.Stat(filepath.Join(current, "scripts", "test-postgres-candidate.sh")); scriptErr == nil {
				return current, nil
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("repository root not found")
		}
		current = parent
	}
}

func RunCandidate(options RunOptions) (CandidateIndex, string, error) {
	root, err := normalizeRoot(options.Root)
	if err != nil {
		return CandidateIndex{}, "", err
	}
	options.Root = root
	if options.CandidateID == "" {
		options.CandidateID = os.Getenv("CANDIDATE_ID")
	}
	if options.NocturneOCILayout != "" {
		options.NocturneOCILayout, err = filepath.Abs(options.NocturneOCILayout)
		if err != nil {
			return CandidateIndex{}, "", err
		}
	}
	if options.EvidenceDir == "" {
		options.EvidenceDir = filepath.Join(os.TempDir(), "edu-agent-operations-evidence")
	}
	options.EvidenceDir, err = filepath.Abs(options.EvidenceDir)
	if err != nil {
		return CandidateIndex{}, "", err
	}
	if err := ensureWritableDirectory(options.EvidenceDir); err != nil {
		return CandidateIndex{}, "", fmt.Errorf("evidence directory is not writable: %w", err)
	}
	options.HostLockPath = resolvedHostLockPath(options.HostLockPath)
	attestor, _, err := LoadOrCreateAttestor(options.AttestationKeyFile, root, options.EvidenceDir)
	if err != nil {
		return CandidateIndex{}, "", err
	}
	dependencies, dependencyDigest, err := LoadDependencyLock(root)
	if err != nil {
		return CandidateIndex{}, "", err
	}
	candidate, err := CandidateFingerprint(root, options.CandidateID, options.EvidenceDir)
	if err != nil {
		return CandidateIndex{}, "", err
	}
	candidateDir := filepath.Join(options.EvidenceDir, candidate)
	if err := ensureWritableDirectory(candidateDir); err != nil {
		return CandidateIndex{}, "", err
	}
	lanes, err := buildLaneDefinitions(root, dependencies, dependencyDigest, options)
	if err != nil {
		return CandidateIndex{}, "", err
	}
	selected, err := selectedLaneSet(lanes, options.SelectedLanes)
	if err != nil {
		return CandidateIndex{}, "", err
	}

	var lock *HostLock
	lockFailure := ""
	if !options.DryRun {
		lock, err = AcquireHostLock(options.HostLockPath)
		if err != nil {
			lockFailure = err.Error()
		} else {
			defer lock.Close()
		}
	}

	platform := currentPlatform()
	prepared := make([]preparedLane, 0, len(lanes))
	for _, lane := range lanes {
		material := keyMaterialForLane(candidate, root, platform, lane)
		key, keyErr := ComputeEvidenceKey(material)
		if keyErr != nil {
			return CandidateIndex{}, "", keyErr
		}
		slug := strings.ReplaceAll(lane.Name, "/", "-")
		relativeManifest := filepath.ToSlash(filepath.Join("lanes", slug, key+".json"))
		relativeLog := filepath.ToSlash(filepath.Join("lanes", slug, key+".log"))
		item := preparedLane{
			definition:       lane,
			material:         material,
			key:              key,
			relativeManifest: relativeManifest,
			relativeLog:      relativeLog,
			manifestPath:     filepath.Join(candidateDir, filepath.FromSlash(relativeManifest)),
			logPath:          filepath.Join(candidateDir, filepath.FromSlash(relativeLog)),
		}
		if _, ok := selected[lane.Name]; ok && lockFailure == "" && options.Resume {
			if _, statErr := os.Stat(item.manifestPath); statErr == nil {
				if _, verifyErr := VerifyPassedEvidence(item.manifestPath, candidateDir, material, attestor); verifyErr == nil {
					item.reused = true
				}
			}
		}
		prepared = append(prepared, item)
	}

	index := CandidateIndex{
		SchemaVersion:        IndexSchemaVersion,
		CandidateFingerprint: candidate,
		GeneratedAt:          time.Now().UTC().Format(time.RFC3339Nano),
		Lanes:                make([]IndexLane, 0, len(lanes)),
	}
	for _, item := range prepared {
		lane := item.definition
		entry := IndexLane{Lane: lane.Name, Scenario: lane.Scenario, EvidenceKey: item.key}
		if _, ok := selected[lane.Name]; !ok {
			entry.Status = StatusNotRun
			entry.Reason = "lane was not selected"
			index.Lanes = append(index.Lanes, entry)
			continue
		}
		if lockFailure != "" {
			entry.Status = StatusBlocked
			entry.Reason = lockFailure
			index.Lanes = append(index.Lanes, entry)
			continue
		}
		if driftErr := verifyLaneInputsStable(options, candidate, item); driftErr != nil {
			entry.Status = StatusFailed
			entry.Reason = "candidate inputs changed before lane processing: " + driftErr.Error()
			index.Lanes = append(index.Lanes, entry)
			continue
		}
		if item.reused {
			if driftErr := verifyLaneInputsStable(options, candidate, item); driftErr != nil {
				entry.Status = StatusFailed
				entry.Reason = "candidate inputs changed while reusing lane evidence: " + driftErr.Error()
			} else {
				entry.Status = StatusReused
				entry.Reason = "matching attested passed evidence and redacted log were verified"
				entry.EvidencePath = item.relativeManifest
			}
			index.Lanes = append(index.Lanes, entry)
			continue
		}
		preflightStatus, preflightReason := preflightLane(lane, dependencies)
		if preflightStatus != StatusPassed {
			now := time.Now().UTC()
			execution := laneExecution{
				Status:   preflightStatus,
				Reason:   preflightReason,
				Tests:    TestRecord{Selected: sortedUnique(lane.SelectedTests)},
				Started:  now,
				Finished: now,
			}
			if err := AtomicWriteFile(item.logPath, []byte(preflightReason+"\n"), 0o600); err != nil {
				return CandidateIndex{}, "", err
			}
			if err := persistEvidence(item.manifestPath, item.relativeLog, item.material, item.key, execution, item.logPath, attestor); err != nil {
				return CandidateIndex{}, "", err
			}
			entry.Status = preflightStatus
			entry.Reason = preflightReason
			entry.EvidencePath = item.relativeManifest
			index.Lanes = append(index.Lanes, entry)
			continue
		}
		if options.DryRun {
			entry.Status = StatusNotRun
			entry.Reason = "dry-run preflight passed; lane was not executed"
			index.Lanes = append(index.Lanes, entry)
			continue
		}
		if driftErr := verifyLaneInputsStable(options, candidate, item); driftErr != nil {
			entry.Status = StatusFailed
			entry.Reason = "candidate inputs changed immediately before lane execution: " + driftErr.Error()
			index.Lanes = append(index.Lanes, entry)
			continue
		}
		execution, runErr := executeLane(root, lane, item.material, item.logPath, lock, options.HostLockPath)
		if runErr != nil {
			return CandidateIndex{}, "", runErr
		}
		if driftErr := verifyLaneInputsStable(options, candidate, item); driftErr != nil {
			execution.Status = StatusFailed
			execution.Reason = "candidate inputs changed during lane execution: " + driftErr.Error()
		}
		if err := persistEvidence(item.manifestPath, item.relativeLog, item.material, item.key, execution, item.logPath, attestor); err != nil {
			return CandidateIndex{}, "", err
		}
		entry.Status = execution.Status
		entry.Reason = execution.Reason
		entry.EvidencePath = item.relativeManifest
		index.Lanes = append(index.Lanes, entry)
	}
	if driftErr := verifyPreparedInputsStable(options, candidate, prepared); driftErr != nil {
		for position := range index.Lanes {
			if _, ok := selected[index.Lanes[position].Lane]; !ok {
				continue
			}
			index.Lanes[position].Status = StatusFailed
			index.Lanes[position].Reason = "candidate inputs changed before candidate index finalization: " + driftErr.Error()
			index.Lanes[position].EvidencePath = ""
		}
	}
	statuses := make([]Status, 0, len(index.Lanes))
	for _, lane := range index.Lanes {
		statuses = append(statuses, lane.Status)
	}
	index.Overall = AggregateStatuses(statuses)
	index.QualificationKey, err = ComputeQualificationKey(candidate, index.Lanes)
	if err != nil {
		return CandidateIndex{}, "", err
	}
	if err := attestor.SignCandidateIndex(&index); err != nil {
		return CandidateIndex{}, "", err
	}
	if err := ValidateCandidateIndex(index); err != nil {
		return CandidateIndex{}, "", err
	}
	indexName := "candidate-index.json"
	if lockFailure != "" {
		indexName = fmt.Sprintf("candidate-index.blocked.%d.json", os.Getpid())
	}
	indexPath := filepath.Join(candidateDir, indexName)
	if err := AtomicWriteJSON(indexPath, index, 0o600); err != nil {
		return CandidateIndex{}, "", err
	}
	return index, indexPath, nil
}

func VerifyCandidate(options VerifyOptions) (CandidateIndex, error) {
	root, err := normalizeRoot(options.Root)
	if err != nil {
		return CandidateIndex{}, err
	}
	options.Root = root
	if options.CandidateID == "" {
		options.CandidateID = os.Getenv("CANDIDATE_ID")
	}
	if options.EvidenceDir == "" {
		options.EvidenceDir = filepath.Join(os.TempDir(), "edu-agent-operations-evidence")
	}
	options.EvidenceDir, err = filepath.Abs(options.EvidenceDir)
	if err != nil {
		return CandidateIndex{}, err
	}
	if options.NocturneOCILayout != "" {
		options.NocturneOCILayout, err = filepath.Abs(options.NocturneOCILayout)
		if err != nil {
			return CandidateIndex{}, err
		}
	}
	options.HostLockPath = resolvedHostLockPath(options.HostLockPath)
	attestor, _, err := LoadAttestor(options.AttestationKeyFile, root, options.EvidenceDir)
	if err != nil {
		return CandidateIndex{}, err
	}
	candidate, err := CandidateFingerprint(root, options.CandidateID, options.EvidenceDir)
	if err != nil {
		return CandidateIndex{}, err
	}
	candidateDir := filepath.Join(options.EvidenceDir, candidate)
	index, err := ReadCandidateIndexStrict(filepath.Join(candidateDir, "candidate-index.json"), attestor)
	if err != nil {
		return CandidateIndex{}, err
	}
	if index.CandidateFingerprint != candidate {
		return CandidateIndex{}, errors.New("candidate index fingerprint is stale")
	}
	dependencies, dependencyDigest, err := LoadDependencyLock(root)
	if err != nil {
		return CandidateIndex{}, err
	}
	runOptions := RunOptions{
		Root:               root,
		EvidenceDir:        options.EvidenceDir,
		NocturneOCILayout:  options.NocturneOCILayout,
		CandidateID:        options.CandidateID,
		HostLockPath:       options.HostLockPath,
		AttestationKeyFile: options.AttestationKeyFile,
	}
	lanes, err := buildLaneDefinitions(root, dependencies, dependencyDigest, runOptions)
	if err != nil {
		return CandidateIndex{}, err
	}
	if len(index.Lanes) != len(lanes) {
		return CandidateIndex{}, errors.New("candidate index lane set is incomplete")
	}
	platform := currentPlatform()
	prepared := make([]preparedLane, 0, len(lanes))
	for position, lane := range lanes {
		entry := index.Lanes[position]
		if entry.Lane != lane.Name || entry.Scenario != lane.Scenario {
			return CandidateIndex{}, errors.New("candidate index lane order or identity changed")
		}
		material := keyMaterialForLane(candidate, root, platform, lane)
		expectedKey, keyErr := ComputeEvidenceKey(material)
		if keyErr != nil {
			return CandidateIndex{}, keyErr
		}
		if entry.EvidenceKey != expectedKey {
			return CandidateIndex{}, fmt.Errorf("candidate index lane %s evidence key does not match current inputs", lane.Name)
		}
		prepared = append(prepared, preparedLane{definition: lane, material: material, key: expectedKey})
		if entry.EvidencePath == "" {
			if entry.Status == StatusPassed || entry.Status == StatusReused {
				return CandidateIndex{}, fmt.Errorf("candidate index lane %s lacks a manifest", lane.Name)
			}
			continue
		}
		manifestPath, pathErr := safeEvidencePath(candidateDir, entry.EvidencePath)
		if pathErr != nil {
			return CandidateIndex{}, pathErr
		}
		if entry.Status == StatusPassed || entry.Status == StatusReused {
			evidence, verifyErr := VerifyPassedEvidence(manifestPath, candidateDir, material, attestor)
			if verifyErr != nil {
				return CandidateIndex{}, fmt.Errorf("verify lane %s: %w", lane.Name, verifyErr)
			}
			if bindingErr := ValidateIndexEvidenceBinding(entry, evidence, expectedKey); bindingErr != nil {
				return CandidateIndex{}, fmt.Errorf("verify lane %s binding: %w", lane.Name, bindingErr)
			}
			continue
		}
		evidence, readErr := ReadEvidenceStrict(manifestPath, attestor)
		if readErr != nil {
			return CandidateIndex{}, fmt.Errorf("verify lane %s manifest: %w", lane.Name, readErr)
		}
		if bindingErr := ValidateIndexEvidenceBinding(entry, evidence, expectedKey); bindingErr != nil || !reflect.DeepEqual(evidence.KeyMaterial(), normalizeKeyMaterial(material)) {
			if bindingErr != nil {
				return CandidateIndex{}, fmt.Errorf("verify lane %s binding: %w", lane.Name, bindingErr)
			}
			return CandidateIndex{}, fmt.Errorf("candidate index lane %s manifest inputs do not match", lane.Name)
		}
	}
	if driftErr := verifyPreparedInputsStable(runOptions, candidate, prepared); driftErr != nil {
		return CandidateIndex{}, fmt.Errorf("candidate inputs changed during verification: %w", driftErr)
	}
	if index.Overall != StatusPassed {
		return index, fmt.Errorf("candidate overall status is %s", index.Overall)
	}
	return index, nil
}

func resolvedHostLockPath(explicit string) string {
	path := explicit
	if path == "" {
		path = os.Getenv("OPERATIONS_CANDIDATE_LOCK_FILE")
	}
	if path == "" {
		path = DefaultHostLockPath
	}
	if absolute, err := filepath.Abs(path); err == nil {
		return absolute
	}
	return path
}

func keyMaterialForLane(candidate, root string, platform PlatformRecord, lane laneDefinition) KeyMaterial {
	return normalizeKeyMaterial(KeyMaterial{
		CandidateFingerprint: candidate,
		Lane:                 lane.Name,
		Scenario:             lane.Scenario,
		Command:              CommandRecord{Argv: append([]string(nil), lane.Argv...), Cwd: root},
		Platform:             platform,
		Toolchain:            collectToolchain(lane.Tools),
		PinnedInputs:         cloneMap(lane.PinnedInputs),
		SelectedTests:        append([]string(nil), lane.SelectedTests...),
		ExpectedGoTests:      append([]string(nil), lane.ExpectedGoTests...),
		ExternalTargets:      append([]string(nil), lane.ExternalTargets...),
		OutputAssertions:     append([]string(nil), lane.OutputAssertions...),
		RequireAnyGoTest:     lane.RequireAnyGoTest,
	})
}

func currentInputMaterials(options RunOptions) (string, map[string]KeyMaterial, error) {
	candidate, err := CandidateFingerprint(options.Root, options.CandidateID, options.EvidenceDir)
	if err != nil {
		return "", nil, err
	}
	dependencies, dependencyDigest, err := LoadDependencyLock(options.Root)
	if err != nil {
		return "", nil, err
	}
	lanes, err := buildLaneDefinitions(options.Root, dependencies, dependencyDigest, options)
	if err != nil {
		return "", nil, err
	}
	platform := currentPlatform()
	materials := make(map[string]KeyMaterial, len(lanes))
	for _, lane := range lanes {
		materials[lane.Name] = keyMaterialForLane(candidate, options.Root, platform, lane)
	}
	return candidate, materials, nil
}

func validateCandidateFingerprintSnapshot(expected, current string) error {
	if current != expected {
		return errors.New("candidate fingerprint changed")
	}
	return nil
}

func verifyLaneInputsStable(options RunOptions, expectedCandidate string, expected preparedLane) error {
	candidate, materials, err := currentInputMaterials(options)
	if err != nil {
		return err
	}
	if err := validateCandidateFingerprintSnapshot(expectedCandidate, candidate); err != nil {
		return err
	}
	material, ok := materials[expected.definition.Name]
	if !ok {
		return fmt.Errorf("lane %s disappeared", expected.definition.Name)
	}
	if !reflect.DeepEqual(material, normalizeKeyMaterial(expected.material)) {
		return fmt.Errorf("lane %s runner, lock, toolchain, or OCI inputs changed", expected.definition.Name)
	}
	key, err := ComputeEvidenceKey(material)
	if err != nil {
		return err
	}
	if key != expected.key {
		return fmt.Errorf("lane %s evidence key changed", expected.definition.Name)
	}
	return nil
}

func verifyPreparedInputsStable(options RunOptions, expectedCandidate string, prepared []preparedLane) error {
	candidate, materials, err := currentInputMaterials(options)
	if err != nil {
		return err
	}
	if err := validateCandidateFingerprintSnapshot(expectedCandidate, candidate); err != nil {
		return err
	}
	for _, expected := range prepared {
		material, ok := materials[expected.definition.Name]
		if !ok {
			return fmt.Errorf("lane %s disappeared", expected.definition.Name)
		}
		if !reflect.DeepEqual(material, normalizeKeyMaterial(expected.material)) {
			return fmt.Errorf("lane %s runner, lock, toolchain, or OCI inputs changed", expected.definition.Name)
		}
		key, keyErr := ComputeEvidenceKey(material)
		if keyErr != nil {
			return keyErr
		}
		if key != expected.key {
			return fmt.Errorf("lane %s evidence key changed", expected.definition.Name)
		}
	}
	return nil
}

func normalizeRoot(root string) (string, error) {
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return FindRepositoryRoot(cwd)
	}
	return filepath.Abs(root)
}

func buildLaneDefinitions(root string, dependencies DependencyLock, dependencyDigest string, options RunOptions) ([]laneDefinition, error) {
	postgresRunnerDigest, _, err := HashFile(filepath.Join(root, "scripts", "test-postgres-candidate.sh"))
	if err != nil {
		return nil, err
	}
	notesyncRunnerDigest, _, err := HashFile(filepath.Join(root, "scripts", "test-notesync-candidate.sh"))
	if err != nil {
		return nil, err
	}
	nocturneRunnerDigest, _, err := HashFile(filepath.Join(root, "contracttests", "nocturne", "run-compose-e2e.sh"))
	if err != nil {
		return nil, err
	}
	common := map[string]string{
		"dependency_index_sha256": dependencyDigest,
		"go_version":              dependencies.GoVersion,
		"host_lock_path":          resolvedHostLockPath(options.HostLockPath),
		"host_lock_protocol":      HostLockProtocol,
	}
	postgresPinned := cloneMap(common)
	postgresPinned["postgres_image"] = dependencies.Postgres.Image
	postgresPinned["postgres_platform"] = dependencies.Postgres.Platform
	postgresPinned["postgres_test_timeout"] = "45m"
	postgresPinned["postgres_tmpfs_size"] = "2g"
	postgresPinned["postgres_test_run"] = "<unset>"
	postgresPinned["runner_sha256"] = postgresRunnerDigest
	postgresTools := []string{"bash", "docker", "flock", "git", "go", "readlink", "sha256sum"}
	postgresEnvironment := map[string]string{
		"POSTGRES_IMAGE":        dependencies.Postgres.Image,
		"POSTGRES_PLATFORM":     dependencies.Postgres.Platform,
		"POSTGRES_TEST_TIMEOUT": "45m",
		"POSTGRES_TMPFS_SIZE":   "2g",
		"POSTGRES_TEST_RUN":     "",
	}
	postgresLane := func(name, shard, description string) laneDefinition {
		return laneDefinition{
			LaneDescription:  LaneDescription{Name: name, Scenario: "postgres-" + shard, Description: description},
			Argv:             []string{"bash", "scripts/test-postgres-candidate.sh", "--shard", shard},
			Environment:      cloneMap(postgresEnvironment),
			Tools:            append([]string(nil), postgresTools...),
			PinnedInputs:     cloneMap(postgresPinned),
			SelectedTests:    []string{"postgres-shard:" + shard},
			RequireAnyGoTest: true,
		}
	}
	modelTest := "TestBlackBoxProductionFakeModelVerticalPostgreSQL"
	offlineTest := "TestBlackBoxOfflineObjectivePrepareLearnSyncStatus"
	model := postgresLane("model-vertical", "model-vertical", "production model client/adapter/application vertical with real PostgreSQL")
	model.Scenario = "production-fake-model-postgresql"
	model.SelectedTests = []string{modelTest}
	model.ExpectedGoTests = []string{modelTest}
	model.RequireAnyGoTest = false
	model.Tools = append(model.Tools, "psql")
	model.Selections = []goSelection{{Cwd: filepath.Join(root, "contracttests", "cli-m1"), Package: "./blackbox", Regex: "^" + modelTest + "$", Expected: []string{modelTest}}}
	offline := postgresLane("offline-blackbox", "offline-blackbox", "real CLI Offline prepare/learn/sync/status black box with PostgreSQL")
	offline.Scenario = "offline-cli-blackbox-postgresql"
	offline.SelectedTests = []string{offlineTest}
	offline.ExpectedGoTests = []string{offlineTest}
	offline.RequireAnyGoTest = false
	offline.Tools = append(offline.Tools, "psql")
	offline.Selections = []goSelection{{Cwd: filepath.Join(root, "contracttests", "cli-m1"), Package: "./blackbox", Regex: "^" + offlineTest + "$", Expected: []string{offlineTest}}}

	notesyncExpected := []string{
		"TestPublicationConsumerCapabilityDependencyAndStaleSuppression",
		"TestPostgreSQLKnowledgeNotesyncRealUpstreamAcceptRemoteRepublishesWithoutLoop",
		"TestRealUpstreamCandidate",
	}
	notesyncPinned := cloneMap(common)
	notesyncPinned["runner_sha256"] = notesyncRunnerDigest
	notesyncPinned["service_image"] = dependencies.NoteSync.ServiceImage
	notesyncPinned["service_version"] = dependencies.NoteSync.ServiceVersion
	notesyncPinned["service_commit"] = dependencies.NoteSync.ServiceCommit
	notesyncPinned["plugin_version"] = dependencies.NoteSync.PluginVersion
	notesyncPinned["plugin_commit"] = dependencies.NoteSync.PluginCommit
	notesyncPinned["authority_spec_sha256"] = dependencies.NoteSyncAuthoritySpecSHA256
	notesyncPinned["platform"] = dependencies.NoteSync.Platform
	notesync := laneDefinition{
		LaneDescription: LaneDescription{Name: "notesync-real", Scenario: "fast-note-sync-3.6.1-real-candidate", Description: "pinned Fast Note Sync real-container compatibility gate"},
		Argv:            []string{"bash", "scripts/test-notesync-candidate.sh"},
		Environment: map[string]string{
			"DOCKER_DEFAULT_PLATFORM": dependencies.NoteSync.Platform,
		},
		Tools:           []string{"bash", "curl", "docker", "flock", "git", "go", "jq", "readlink", "sha256sum"},
		PinnedInputs:    notesyncPinned,
		SelectedTests:   append(append([]string(nil), notesyncExpected...), "notesync-real-contract"),
		ExpectedGoTests: notesyncExpected,
		ExternalTargets: []string{"notesync-real-contract"},
		Selections: []goSelection{
			{Cwd: filepath.Join(root, "server"), Package: "./internal/integrations/notesync", Regex: "^(TestRealUpstreamCandidate|TestPublicationConsumerCapabilityDependencyAndStaleSuppression)$", Expected: []string{"TestRealUpstreamCandidate", "TestPublicationConsumerCapabilityDependencyAndStaleSuppression"}},
			{Cwd: filepath.Join(root, "server"), Package: "./internal/knowledge/postgresstore", Regex: "^TestPostgreSQLKnowledgeNotesyncRealUpstreamAcceptRemoteRepublishesWithoutLoop$", Expected: []string{"TestPostgreSQLKnowledgeNotesyncRealUpstreamAcceptRemoteRepublishesWithoutLoop"}},
		},
		OutputAssertions: []string{dependencies.NoteSync.ServiceImage, "NoteSync candidate observed version: " + dependencies.NoteSync.ServiceVersion},
	}

	layoutDigest := "missing"
	layoutArgument := "__MISSING_VERIFIED_OCI_LAYOUT__"
	if options.NocturneOCILayout != "" {
		layoutArgument = options.NocturneOCILayout
		if digest, hashErr := HashTree(options.NocturneOCILayout); hashErr == nil {
			layoutDigest = digest
		} else if !errors.Is(hashErr, os.ErrNotExist) {
			return nil, hashErr
		}
	}
	nocturneExpected := []string{
		"TestManagedBackupErasureVerificationDestroyedArtifactSucceedsAndLiveKeyFails",
		"TestManagedBackupPrecisePruneSuccess",
		"TestManagedBackupRoundTripChunkBoundariesAndDestroyedRestore",
	}
	nocturnePinned := cloneMap(common)
	nocturnePinned["runner_sha256"] = nocturneRunnerDigest
	nocturnePinned["platform"] = dependencies.Nocturne.Platform
	nocturnePinned["platform_manifest_digest"] = dependencies.Nocturne.PlatformManifestDigest
	nocturnePinned["config_digest"] = dependencies.Nocturne.ConfigDigest
	nocturnePinned["image_lock_sha256"] = dependencies.Nocturne.ImageLockSHA256
	nocturnePinned["supply_chain_lock_sha256"] = dependencies.Nocturne.SupplyChainLockSHA256
	nocturnePinned["verified_oci_layout_sha256"] = layoutDigest
	nocturne := laneDefinition{
		LaneDescription: LaneDescription{Name: "nocturne-compose", Scenario: "nocturne-verified-oci-compose-full", Description: "verified Nocturne OCI layout and full Compose/PostgreSQL gate"},
		Argv:            []string{"sh", "contracttests/nocturne/run-compose-e2e.sh", layoutArgument, "full"},
		Environment:     map[string]string{},
		Tools:           []string{"docker", "docker-compose", "flock", "go", "python3", "readlink", "skopeo"},
		PinnedInputs:    nocturnePinned,
		SelectedTests:   append(append([]string(nil), nocturneExpected...), "nocturne-compose:full"),
		ExpectedGoTests: nocturneExpected,
		ExternalTargets: []string{"nocturne-compose:full"},
		OutputAssertions: []string{
			"Nocturne Compose candidate: PASS scenario=full",
		},
		Selections: []goSelection{{
			Cwd: root + string(filepath.Separator) + "server", Package: "./internal/integrations/nocturne",
			Regex: "^(TestManagedBackupRoundTripChunkBoundariesAndDestroyedRestore|TestManagedBackupErasureVerificationDestroyedArtifactSucceedsAndLiveKeyFails|TestManagedBackupPrecisePruneSuccess)$", Expected: nocturneExpected,
		}},
	}

	dbCoreExpected := []string{
		"TestPostgreSQLKnowledgeFullTreeRebuildParityAndFailClosedCorpus",
		"TestPostgreSQLIdentityPairingConcurrentSingleUse",
		"TestPostgreSQLIdentityPairingReplayRejectedAfterCommit",
		"TestPostgreSQLIdentityPairingTransactionRollbackAtEveryWrite",
		"TestPostgreSQLIdentityRevokeTransactionRollsBackTokenFailure",
		"TestPostgreSQLIdentityRevokeAuthenticationRaceFencesAfterCommit",
		"TestPostgreSQLOutboxConcurrentClaimHasSingleLeaseOwner",
		"TestPostgreSQLOutboxExpiredLeaseReclaimFencesPreviousOwner",
		"TestPostgreSQLOutboxWorkerReclaimConvergesCommittedIdempotentSideEffect",
		"TestPostgreSQLOutboxClaimFailureDoesNotConsumeMessage",
		"TestPostgreSQLOutboxTransitionWriteFaultsRollbackAtomically",
		"TestPostgreSQLOutboxPrivacyRedactionRevokesActiveLease",
	}
	dbCore := postgresLane("postgres-db-core", "db-core", "application, knowledge, identity, outbox, platform and migration PostgreSQL contracts")
	dbCore.ExpectedGoTests = dbCoreExpected
	dbCore.SelectedTests = append(dbCore.SelectedTests, dbCoreExpected...)
	dbCore.RequireAnyGoTest = false
	dbCore.Selections = []goSelection{
		{Cwd: filepath.Join(root, "server"), Package: "./internal/knowledge/postgresstore", Regex: "^TestPostgreSQLKnowledgeFullTreeRebuildParityAndFailClosedCorpus$", Expected: dbCoreExpected[:1]},
		{Cwd: filepath.Join(root, "server"), Package: "./internal/identity/postgresstore", Regex: "^TestPostgreSQLIdentity", Expected: dbCoreExpected[1:6]},
		{Cwd: filepath.Join(root, "server"), Package: "./internal/platform/outbox/postgresstore", Regex: "^TestPostgreSQLOutbox", Expected: dbCoreExpected[6:]},
	}
	learningCoreTests := []string{
		"TestPostgreSQLLearningFullProjectionReplayParityAndFailClosedCorpus",
		"TestPostgreSQLLearningAuthoritativeWriteGroupResponseLossRetryAndWorkerRestart",
	}
	learningCore := postgresLane("postgres-learning-core", "learning-core", "learning PostgreSQL core contracts")
	learningCore.ExpectedGoTests = append([]string(nil), learningCoreTests...)
	learningCore.SelectedTests = append(learningCore.SelectedTests, learningCoreTests...)
	learningCore.RequireAnyGoTest = false
	learningCore.Selections = []goSelection{{
		Cwd: filepath.Join(root, "server"), Package: "./internal/learning/postgresstore",
		Regex:    "^(TestPostgreSQLLearningFullProjectionReplayParityAndFailClosedCorpus|TestPostgreSQLLearningAuthoritativeWriteGroupResponseLossRetryAndWorkerRestart)$",
		Expected: learningCoreTests,
	}}
	learningOfflineTest := "TestPostgreSQLOfflineCredentialEpochFenceAndRevocationRace"
	learningOffline := postgresLane("postgres-learning-offline", "learning-offline", "learning Offline PostgreSQL contracts")
	learningOffline.ExpectedGoTests = []string{learningOfflineTest}
	learningOffline.SelectedTests = append(learningOffline.SelectedTests, learningOfflineTest)
	learningOffline.RequireAnyGoTest = false
	learningOffline.Selections = []goSelection{{Cwd: filepath.Join(root, "server"), Package: "./internal/learning/postgresstore", Regex: "^" + learningOfflineTest + "$", Expected: []string{learningOfflineTest}}}

	return []laneDefinition{
		model,
		dbCore,
		learningCore,
		learningOffline,
		postgresLane("postgres-learning-fault", "learning-fault", "learning typed-record fault matrix"),
		postgresLane("postgres-memory", "memory", "memory PostgreSQL contracts"),
		postgresLane("postgres-privacy-core", "privacy-core", "privacy PostgreSQL core contracts"),
		postgresLane("postgres-privacy-fault", "privacy-fault", "privacy scrub fault matrix"),
		offline,
		notesync,
		nocturne,
	}, nil
}

func selectedLaneSet(lanes []laneDefinition, requested []string) (map[string]struct{}, error) {
	available := make(map[string]struct{}, len(lanes))
	for _, lane := range lanes {
		available[lane.Name] = struct{}{}
	}
	if len(requested) == 0 {
		return available, nil
	}
	selected := make(map[string]struct{}, len(requested))
	for _, name := range requested {
		if _, ok := available[name]; !ok {
			return nil, fmt.Errorf("unknown lane %q", name)
		}
		selected[name] = struct{}{}
	}
	return selected, nil
}

func preflightLane(lane laneDefinition, dependencies DependencyLock) (Status, string) {
	if lane.Name == "nocturne-compose" {
		layout := lane.Argv[2]
		if layout == "__MISSING_VERIFIED_OCI_LAYOUT__" {
			return StatusBlocked, "Nocturne requires --nocturne-oci-layout with a verified OCI layout"
		}
		if info, err := os.Stat(layout); err != nil || !info.IsDir() {
			return StatusBlocked, "Nocturne verified OCI layout is unavailable"
		}
		if runtime.GOOS+"/"+runtime.GOARCH != dependencies.Nocturne.Platform {
			return StatusBlocked, "Nocturne locked platform is " + dependencies.Nocturne.Platform
		}
	}
	for _, tool := range lane.Tools {
		if tool == "docker-compose" {
			continue
		}
		if _, err := exec.LookPath(tool); err != nil {
			return StatusBlocked, "required tool is unavailable: " + tool
		}
	}
	goVersion := collectToolVersion("go")
	if !strings.Contains(goVersion, dependencies.GoVersion) {
		return StatusBlocked, "required Go toolchain is unavailable: " + dependencies.GoVersion
	}
	if containsString(lane.Tools, "docker") {
		if err := exec.Command("docker", "info").Run(); err != nil {
			return StatusBlocked, "Docker daemon is unavailable"
		}
	}
	if containsString(lane.Tools, "docker-compose") {
		if err := exec.Command("docker", "compose", "version").Run(); err != nil {
			return StatusBlocked, "Docker Compose is unavailable"
		}
	}
	for _, selection := range lane.Selections {
		if err := verifyGoSelection(selection); err != nil {
			return StatusFailed, err.Error()
		}
	}
	return StatusPassed, "preflight passed"
}

func verifyGoSelection(selection goSelection) error {
	command := exec.Command("go", "test", "-list", selection.Regex, selection.Package)
	command.Dir = selection.Cwd
	command.Env = commandEnvironment(map[string]string{"GOFLAGS": ""})
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("enumerate Go tests in %s: %w", selection.Package, err)
	}
	listed := make(map[string]struct{})
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		name := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(name, "Test") && !strings.ContainsAny(name, " \t") {
			listed[name] = struct{}{}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(listed) == 0 {
		return fmt.Errorf("Go test selection is empty for %s", selection.Package)
	}
	for _, expected := range selection.Expected {
		if _, ok := listed[expected]; !ok {
			return fmt.Errorf("selected Go test is missing from %s: %s", selection.Package, expected)
		}
	}
	return nil
}

func executeLane(root string, lane laneDefinition, material KeyMaterial, logPath string, lock *HostLock, lockPath string) (laneExecution, error) {
	started := time.Now().UTC()
	runnerWork, err := os.MkdirTemp("", "edu-agent-operations-run-*")
	if err != nil {
		return laneExecution{}, err
	}
	defer os.RemoveAll(runnerWork)
	environment := cloneMap(lane.Environment)
	environment["GOFLAGS"] = ""
	environment["TMPDIR"] = runnerWork
	environment["OPERATIONS_CANDIDATE_LOCK_FILE"] = lockPath
	if len(lane.Argv) >= 2 && lane.Argv[0] == "bash" && lane.Argv[1] == "scripts/test-postgres-candidate.sh" && len(lane.ExpectedGoTests) > 0 {
		environment["OPERATIONS_EXPECTED_GO_TESTS"] = strings.Join(sortedUnique(lane.ExpectedGoTests), ",")
	}
	if strings.HasPrefix(lane.Name, "postgres-") || lane.Name == "model-vertical" || lane.Name == "offline-blackbox" {
		environment["POSTGRES_EVIDENCE_DIR"] = filepath.Join(runnerWork, "postgres-evidence")
	}
	if lane.Name == "nocturne-compose" {
		environment["NOCTURNE_E2E_SERVER_IMAGE"] = ""
		environment["NOCTURNE_E2E_GATE_LOG"] = filepath.Join(runnerWork, "nocturne-gate.log")
		environment["NOCTURNE_E2E_COMPOSE_LOG"] = filepath.Join(runnerWork, "nocturne-compose.log")
	}
	exit, unsafe, captureErr := captureCommand(root, lane.Argv, environment, logPath, lock)
	finished := time.Now().UTC()
	execution := laneExecution{
		Exit:     ExitRecord{Started: exit != nil, Code: exit},
		Tests:    TestRecord{Selected: sortedUnique(lane.SelectedTests)},
		Started:  started,
		Finished: finished,
	}
	if captureErr != nil {
		execution.Status = StatusFailed
		execution.Reason = "command capture failed: " + captureErr.Error()
		return execution, nil
	}
	if unsafe {
		execution.Status = StatusFailed
		execution.Reason = "unsafe durable log content was redacted and rejected"
		return execution, nil
	}
	if exit == nil {
		execution.Status = StatusFailed
		execution.Reason = "command did not start"
		return execution, nil
	}
	if *exit != 0 {
		logContent, _ := os.ReadFile(logPath)
		if *exit == 2 && blockedRunnerOutput(string(logContent)) {
			execution.Status = StatusBlocked
			execution.Reason = "runner prerequisite was unavailable"
		} else {
			execution.Status = StatusFailed
			execution.Reason = "runner exited with code " + strconv.Itoa(*exit)
		}
		return execution, nil
	}
	content, err := os.ReadFile(logPath)
	if err != nil {
		return laneExecution{}, err
	}
	coverage, err := AnalyzeLogCoverage(content, material)
	if err != nil {
		execution.Status = StatusFailed
		execution.Reason = "test evidence verification failed: " + err.Error()
		return execution, nil
	}
	execution.Tests = coverage
	execution.Status = StatusPassed
	execution.Reason = "command completed and required scenarios passed"
	return execution, nil
}

func captureCommand(root string, argv []string, environment map[string]string, logPath string, lock *HostLock) (*int, bool, error) {
	if len(argv) == 0 {
		return nil, false, errors.New("empty command")
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, false, err
	}
	temp, err := os.CreateTemp(filepath.Dir(logPath), ".log-*")
	if err != nil {
		return nil, false, err
	}
	tempName := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempName)
	}
	if err := temp.Chmod(0o600); err != nil {
		cleanup()
		return nil, false, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		cleanup()
		return nil, false, err
	}
	command, commandErr := allowedLaneCommand(argv)
	if commandErr != nil {
		cleanup()
		_ = reader.Close()
		_ = writer.Close()
		return nil, false, commandErr
	}
	command.Dir = root
	command.Stdout = writer
	command.Stderr = writer
	inheritedFD, inheritErr := lock.ConfigureChild(command)
	if inheritErr != nil {
		cleanup()
		_ = reader.Close()
		_ = writer.Close()
		return nil, false, inheritErr
	}
	environment["OPERATIONS_CANDIDATE_LOCK_FD"] = strconv.Itoa(inheritedFD)
	environment["OPERATIONS_CANDIDATE_LOCK_PROTOCOL"] = HostLockProtocol
	command.Env = commandEnvironment(environment)
	startErr := command.Start()
	_ = writer.Close()
	if startErr != nil {
		redacted := RedactText(startErr.Error())
		_, _ = io.WriteString(temp, redacted.Text+"\n")
		_ = reader.Close()
		if err := finalizeAtomicTemp(temp, tempName, logPath); err != nil {
			return nil, redacted.Unsafe, err
		}
		return nil, redacted.Unsafe, nil
	}
	unsafe, redactErr := RedactStream(reader, temp)
	_ = reader.Close()
	waitErr := command.Wait()
	if redactErr != nil {
		cleanup()
		return nil, unsafe, redactErr
	}
	if err := finalizeAtomicTemp(temp, tempName, logPath); err != nil {
		return nil, unsafe, err
	}
	exitCode := 0
	if waitErr != nil {
		var exitError *exec.ExitError
		if errors.As(waitErr, &exitError) {
			exitCode = exitError.ExitCode()
		} else {
			return nil, unsafe, waitErr
		}
	}
	return &exitCode, unsafe, nil
}

func allowedLaneCommand(argv []string) (*exec.Cmd, error) {
	if len(argv) < 2 {
		return nil, errors.New("candidate lane command is incomplete")
	}
	switch {
	case argv[0] == "bash" && argv[1] == "scripts/test-postgres-candidate.sh":
		return exec.Command("bash", argv[1:]...), nil
	case argv[0] == "bash" && argv[1] == "scripts/test-notesync-candidate.sh":
		return exec.Command("bash", argv[1:]...), nil
	case argv[0] == "sh" && argv[1] == "contracttests/nocturne/run-compose-e2e.sh":
		return exec.Command("sh", argv[1:]...), nil
	default:
		return nil, fmt.Errorf("candidate lane command is not approved: %q", strings.Join(argv, " "))
	}
}

func finalizeAtomicTemp(temp *os.File, tempName, target string) error {
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempName)
		return err
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return err
	}
	if err := os.Rename(tempName, target); err != nil {
		_ = os.Remove(tempName)
		return err
	}
	directory, err := os.Open(filepath.Dir(target))
	if err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func persistEvidence(manifestPath, relativeLog string, material KeyMaterial, key string, execution laneExecution, logPath string, attestor *Attestor) error {
	digest, size, err := HashFile(logPath)
	if err != nil {
		return err
	}
	evidence := Evidence{
		SchemaVersion:        EvidenceSchemaVersion,
		EvidenceKey:          key,
		Attempt:              nextEvidenceAttempt(manifestPath, attestor),
		CandidateFingerprint: material.CandidateFingerprint,
		Lane:                 material.Lane,
		Scenario:             material.Scenario,
		Command:              material.Command,
		Status:               execution.Status,
		Reason:               execution.Reason,
		Exit:                 execution.Exit,
		StartedAt:            execution.Started.Format(time.RFC3339Nano),
		FinishedAt:           execution.Finished.Format(time.RFC3339Nano),
		Platform:             material.Platform,
		Toolchain:            cloneMap(material.Toolchain),
		PinnedInputs:         cloneMap(material.PinnedInputs),
		Tests: TestRecord{
			Selected:         sortedUnique(material.SelectedTests),
			ExpectedGo:       sortedUnique(material.ExpectedGoTests),
			ExternalTargets:  sortedUnique(material.ExternalTargets),
			OutputAssertions: sortedUnique(material.OutputAssertions),
			RequireAnyGoTest: material.RequireAnyGoTest,
			Executed:         sortedUnique(execution.Tests.Executed),
			Passed:           sortedUnique(execution.Tests.Passed),
			Failed:           sortedUnique(execution.Tests.Failed),
			Skipped:          sortedUnique(execution.Tests.Skipped),
		},
		Log: LogRecord{Path: relativeLog, SHA256: digest, Bytes: size},
	}
	if err := attestor.SignEvidence(&evidence); err != nil {
		return err
	}
	if err := ValidateEvidence(evidence); err != nil {
		return err
	}
	return AtomicWriteJSON(manifestPath, evidence, 0o600)
}

func nextEvidenceAttempt(path string, attestor *Attestor) int {
	previous, err := ReadEvidenceStrict(path, attestor)
	if err != nil || previous.Attempt < 1 || previous.Attempt == int(^uint(0)>>1) {
		return 1
	}
	return previous.Attempt + 1
}

func collectToolchain(tools []string) map[string]string {
	result := make(map[string]string, len(tools)+1)
	for _, tool := range sortedUnique(tools) {
		result[tool] = collectToolVersion(tool)
	}
	return result
}

func collectToolVersion(tool string) string {
	var command *exec.Cmd
	switch tool {
	case "docker-compose":
		command = exec.Command("docker", "compose", "version")
	case "go":
		command = exec.Command("go", "version")
	case "docker":
		command = exec.Command("docker", "--version")
	case "python3":
		command = exec.Command("python3", "--version")
	case "bash":
		command = exec.Command("bash", "--version")
	case "sha256sum":
		command = exec.Command("sha256sum", "--version")
	case "readlink":
		command = exec.Command("readlink", "--version")
	case "flock":
		command = exec.Command("flock", "--version")
	case "git":
		command = exec.Command("git", "--version")
	case "curl":
		command = exec.Command("curl", "--version")
	case "jq":
		command = exec.Command("jq", "--version")
	case "skopeo":
		command = exec.Command("skopeo", "--version")
	case "psql":
		command = exec.Command("psql", "--version")
	default:
		return "unavailable"
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return "unavailable"
	}
	line := strings.TrimSpace(string(output))
	if newline := strings.IndexByte(line, '\n'); newline >= 0 {
		line = line[:newline]
	}
	if line == "" {
		return "unavailable"
	}
	return line
}

func currentPlatform() PlatformRecord {
	kernel := runtime.GOOS + "/" + runtime.GOARCH
	if output, err := exec.Command("uname", "-srm").Output(); err == nil && strings.TrimSpace(string(output)) != "" {
		kernel = strings.TrimSpace(string(output))
	}
	return PlatformRecord{OS: runtime.GOOS, Arch: runtime.GOARCH, Runtime: runtime.Version(), Kernel: kernel}
}

func ensureWritableDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	probe, err := os.CreateTemp(path, ".write-probe-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

func commandEnvironment(overrides map[string]string) []string {
	result := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, replaced := overrides[key]; replaced || sensitiveEnvironmentKey(key) {
			continue
		}
		result = append(result, entry)
	}
	return append(result, formatEnvironment(overrides)...)
}

func sensitiveEnvironmentKey(key string) bool {
	upper := strings.ToUpper(key)
	if strings.HasPrefix(upper, "MODEL_") || strings.HasPrefix(upper, "OPENAI_") || strings.HasPrefix(upper, "ANTHROPIC_") {
		return true
	}
	if upper == "TEST_DATABASE_URL" || upper == "AUTHORIZATION" {
		return true
	}
	for _, marker := range []string{"TOKEN", "PASSWORD", "PASSPHRASE", "SECRET", "API_KEY", "PAIRING", "CREDENTIAL", "PRIVATE_KEY", "CHALLENGE_KEYS", "ANSWER", "KNOWLEDGE_BODY"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

func formatEnvironment(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func blockedRunnerOutput(output string) bool {
	for _, marker := range []string{
		"required command is unavailable",
		"required tool not found",
		"Docker daemon is unavailable",
		"another host-wide candidate gate holds",
		"another PostgreSQL candidate runner holds",
	} {
		if strings.Contains(output, marker) {
			return true
		}
	}
	return false
}
