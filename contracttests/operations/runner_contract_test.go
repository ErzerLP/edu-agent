package operations

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestHeavyRunnersShareCanonicalHostLockContract(t *testing.T) {
	root, err := FindRepositoryRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{
		filepath.Join(root, "scripts", "test-postgres-candidate.sh"),
		filepath.Join(root, "scripts", "test-notesync-candidate.sh"),
		filepath.Join(root, "contracttests", "nocturne", "run-compose-e2e.sh"),
	}
	for _, path := range paths {
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		text := string(content)
		if !strings.Contains(text, "export GOFLAGS=''") {
			t.Fatalf("%s does not clear inherited GOFLAGS", path)
		}
		for _, required := range []string{
			"OPERATIONS_CANDIDATE_LOCK_FILE",
			"OPERATIONS_CANDIDATE_LOCK_FD",
			"OPERATIONS_CANDIDATE_LOCK_PROTOCOL",
			DefaultHostLockPath,
			HostLockProtocol,
		} {
			if !strings.Contains(text, required) {
				t.Fatalf("%s does not implement shared lock contract %q", path, required)
			}
		}
		for _, obsolete := range []string{"POSTGRES_CANDIDATE_LOCK_FILE", "edu-agent-postgres-candidate.lock", "edu-agent-notesync-candidate.lock"} {
			if strings.Contains(text, obsolete) {
				t.Fatalf("%s retains obsolete private lock %q", path, obsolete)
			}
		}
	}

	nocturne, err := os.ReadFile(paths[2])
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"$TMP_DIR/gate.log", "$TMP_DIR/compose.log", `rm -rf "$TMP_DIR"`, "go test -json ./internal/integrations/nocturne"} {
		if !strings.Contains(string(nocturne), expected) {
			t.Fatalf("Nocturne runner lacks unique temporary log cleanup contract %q", expected)
		}
	}
	notesync, err := os.ReadFile(paths[1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(notesync), "go test -json -count=1") {
		t.Fatal("NoteSync runner does not emit explicit Go JSON events")
	}
	postgres, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"command -v psql", "SHARD_SELECTION_FILES", `if [[ -n "$TEST_RUN_REGEX" ]]`} {
		if !strings.Contains(string(postgres), required) {
			t.Fatalf("PostgreSQL runner lacks narrowed selection contract %q", required)
		}
	}
}

func TestOperationsWrapperPreservesSubcommandPosition(t *testing.T) {
	root, err := FindRepositoryRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err := os.ReadFile(filepath.Join(root, "scripts", "test-operations-candidate.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`case "${1:-}" in`,
		`command=("$BINARY" verify --root "$ROOT" "${@:2}")`,
		`command=("$BINARY" "$@")`,
		`command=("$BINARY" run --root "$ROOT" "${@:2}")`,
	} {
		if !strings.Contains(string(wrapper), required) {
			t.Fatalf("operations wrapper lacks subcommand dispatch contract %q", required)
		}
	}
}

func TestCoordinatorHostLockInheritanceDoesNotRelock(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("host lock inheritance is Linux-only")
	}
	lockPath := filepath.Join(t.TempDir(), "operations.lock")
	lock, err := AcquireHostLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if competing, competingErr := AcquireHostLock(lockPath); competingErr == nil {
		_ = competing.Close()
		t.Fatal("independent runner acquired an already-held host lock")
	}

	command := exec.Command("bash", "-c", `set -euo pipefail
fd=$OPERATIONS_CANDIDATE_LOCK_FD
[[ $OPERATIONS_CANDIDATE_LOCK_PROTOCOL == inherited-fd-v1 ]]
[[ $(readlink -f -- "/proc/$$/fd/$fd") == $(readlink -f -- "$OPERATIONS_CANDIDATE_LOCK_FILE") ]]
flock -n "$fd"
printf inherited-lock-ok`)
	fd, err := lock.ConfigureChild(command)
	if err != nil {
		t.Fatal(err)
	}
	command.Env = append(os.Environ(),
		"OPERATIONS_CANDIDATE_LOCK_FILE="+lockPath,
		fmt.Sprintf("OPERATIONS_CANDIDATE_LOCK_FD=%d", fd),
		"OPERATIONS_CANDIDATE_LOCK_PROTOCOL="+HostLockProtocol,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("inherited lock failed: %v: %s", err, output)
	}
	if string(output) != "inherited-lock-ok" {
		t.Fatalf("unexpected inherited lock output %q", output)
	}
}

func TestResolvedHostLockUsesCanonicalEnvironmentOverride(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "custom.lock")
	t.Setenv("OPERATIONS_CANDIDATE_LOCK_FILE", custom)
	if got := resolvedHostLockPath(""); got != custom {
		t.Fatalf("resolved lock path=%q want=%q", got, custom)
	}
	if got := resolvedHostLockPath("/explicit.lock"); got != "/explicit.lock" {
		t.Fatalf("explicit lock path=%q", got)
	}
}
