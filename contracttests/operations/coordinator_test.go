package operations

import (
	"slices"
	"strings"
	"testing"
)

func TestLaneDefinitionsProduceValidEvidenceKeys(t *testing.T) {
	root, err := FindRepositoryRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	dependencies, dependencyDigest, err := LoadDependencyLock(root)
	if err != nil {
		t.Fatal(err)
	}
	lanes, err := buildLaneDefinitions(root, dependencies, dependencyDigest, RunOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	wantLanes := []string{
		"model-vertical",
		"postgres-db-core",
		"postgres-learning-core",
		"postgres-learning-offline",
		"postgres-learning-fault",
		"postgres-memory",
		"postgres-privacy-core",
		"postgres-privacy-fault",
		"offline-blackbox",
		"notesync-real",
		"nocturne-compose",
	}
	if len(lanes) != len(wantLanes) {
		t.Fatalf("lane count=%d want=%d", len(lanes), len(wantLanes))
	}

	platform := currentPlatform()
	for position, lane := range lanes {
		if lane.Name != wantLanes[position] {
			t.Fatalf("lane[%d]=%s want=%s", position, lane.Name, wantLanes[position])
		}
		for _, expected := range lane.ExpectedGoTests {
			if !slices.Contains(lane.SelectedTests, expected) {
				t.Fatalf("lane %s expected Go target %s is absent from selected targets", lane.Name, expected)
			}
		}
		material := KeyMaterial{
			CandidateFingerprint: strings.Repeat("a", 64),
			Lane:                 lane.Name,
			Scenario:             lane.Scenario,
			Command:              CommandRecord{Argv: append([]string(nil), lane.Argv...), Cwd: root},
			Platform:             platform,
			Toolchain:            map[string]string{"go": dependencies.GoVersion},
			PinnedInputs:         cloneMap(lane.PinnedInputs),
			SelectedTests:        append([]string(nil), lane.SelectedTests...),
			ExpectedGoTests:      append([]string(nil), lane.ExpectedGoTests...),
			ExternalTargets:      append([]string(nil), lane.ExternalTargets...),
			OutputAssertions:     append([]string(nil), lane.OutputAssertions...),
			RequireAnyGoTest:     lane.RequireAnyGoTest,
		}
		if _, err := ComputeEvidenceKey(material); err != nil {
			t.Fatalf("lane %s evidence material: %v", lane.Name, err)
		}
	}

	status, reason := preflightLane(lanes[len(lanes)-1], dependencies)
	if status != StatusBlocked || !strings.Contains(reason, "--nocturne-oci-layout") {
		t.Fatalf("missing Nocturne layout status=%s reason=%q", status, reason)
	}
}
