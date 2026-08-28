package blackbox

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	fakeAPIKey      = "blackbox-fake-api-key"
	fakeControlKey  = "blackbox-fake-control-key"
	proxyControlKey = "blackbox-proxy-control-key"
)

var (
	binaryRoot           string
	serverBin            string
	cliBin               string
	fakeLLMBin           string
	proxyBin             string
	projectionRebuildBin string
	errorCodeRE          = regexp.MustCompile(`error\[([a-z0-9_]+)\]`)
)

type commandResult struct {
	stdout []byte
	stderr []byte
	exit   int
}

type runningProcess struct {
	name string
	cmd  *exec.Cmd
	done chan error
	once sync.Once
}

type harness struct {
	t             *testing.T
	schema        string
	runtimeDSN    string
	serverURL     string
	fakeURL       string
	serverEnv     []string
	serverLogPath string
	primaryHome   string
	secondaryHome string
	primaryDevice string
	secondDevice  string
	proxyURL      string
}

type fakeScenario struct {
	Kind                 string   `json:"kind"`
	RiskFlag             string   `json:"risk_flag,omitempty"`
	StatusCode           int      `json:"status_code,omitempty"`
	DelayMillis          int64    `json:"delay_ms,omitempty"`
	RetryAfter           string   `json:"retry_after,omitempty"`
	ActivityType         string   `json:"activity_type,omitempty"`
	AllowedHelp          []string `json:"allowed_help,omitempty"`
	AssessmentConclusion string   `json:"assessment_conclusion,omitempty"`
	RouteStepLimit       int      `json:"route_step_limit,omitempty"`
}

type proxyRule struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	OperationID string `json:"operation_id"`
}

type proxyRuleStats struct {
	Rule          proxyRule `json:"rule"`
	Calls         int       `json:"calls"`
	UpstreamCalls int       `json:"upstream_calls"`
	Drops         int       `json:"drops"`
	Rejections    int       `json:"rejections"`
}

type proxyAuditEntry struct {
	Sequence       uint64 `json:"sequence"`
	Method         string `json:"method"`
	Path           string `json:"path"`
	OperationID    string `json:"operation_id,omitempty"`
	RequestBytes   int    `json:"request_bytes"`
	RequestSHA256  string `json:"request_sha256,omitempty"`
	UpstreamStatus int    `json:"upstream_status,omitempty"`
	Dropped        bool   `json:"dropped"`
	Rejected       bool   `json:"rejected"`
	Error          string `json:"error,omitempty"`
}

func TestMain(m *testing.M) {
	if strings.TrimSpace(os.Getenv("TEST_DATABASE_URL")) == "" {
		os.Exit(m.Run())
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Fprintln(os.Stderr, "blackbox setup failed: repository root unavailable")
		os.Exit(1)
	}
	repo := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	var err error
	binaryRoot, err = os.MkdirTemp("", "edu-agent-cli-m1-blackbox-bins-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "blackbox setup failed: temporary binary directory")
		os.Exit(1)
	}
	defer os.RemoveAll(binaryRoot)
	serverBin = filepath.Join(binaryRoot, "edu-agentd")
	cliBin = filepath.Join(binaryRoot, "edu-agent")
	fakeLLMBin = filepath.Join(binaryRoot, "fakellm")
	proxyBin = filepath.Join(binaryRoot, "response-loss-proxy")
	projectionRebuildBin = filepath.Join(binaryRoot, "projection-rebuild-fixture")
	builds := []struct {
		name string
		dir  string
		out  string
		pkg  string
		env  []string
		tags string
	}{
		{name: "edu-agentd", dir: filepath.Join(repo, "server"), out: serverBin, pkg: "./cmd/edu-agentd"},
		{name: "edu-agent", dir: filepath.Join(repo, "clients", "cli-go"), out: cliBin, pkg: "./cmd/edu-agent", env: []string{"CGO_ENABLED=0"}},
		{name: "fakellm", dir: filepath.Join(repo, "contracttests", "fakellm"), out: fakeLLMBin, pkg: "."},
		{name: "response-loss-proxy", dir: filepath.Join(repo, "contracttests", "cli-m1"), out: proxyBin, pkg: "./cmd/response-loss-proxy"},
		{name: "projection-rebuild-fixture", dir: filepath.Join(repo, "server"), out: projectionRebuildBin, pkg: "./contracttests/cli-m1-rebuild", tags: "cli_m1_contract"},
	}
	for _, build := range builds {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		args := []string{"build", "-trimpath"}
		if build.tags != "" {
			args = append(args, "-tags", build.tags)
		}
		args = append(args, "-o", build.out, build.pkg)
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = build.dir
		cmd.Env = append(os.Environ(), build.env...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "blackbox setup failed: build %s\n", build.name)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

func newHarness(t *testing.T) *harness {
	return newHarnessWithOfflineSigner(t, true)
}

func newHarnessWithOfflineSigner(t *testing.T, offlineSigner bool) *harness {
	t.Helper()
	baseDSN := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if baseDSN == "" {
		t.Skip("TEST_DATABASE_URL is not set; CLI M1 PostgreSQL black-box scenarios skipped")
	}
	schema := "cli_m1_bb_" + randomHex(t, 8)
	runPSQL(t, baseDSN, "database setup", "CREATE SCHEMA "+schema)
	t.Cleanup(func() {
		runPSQLCleanup(t, baseDSN, "DROP SCHEMA "+schema+" CASCADE")
	})

	runtimeDSN := withSearchPath(t, baseDSN, schema)
	fakeAddr := freeAddress(t)
	fakeURL := "http://" + fakeAddr
	fakeLog := openProcessLog(t, "fakellm")
	startProcess(t, "fakellm", fakeLLMBin, nil, append(os.Environ(),
		"FAKE_LLM_ADDR="+fakeAddr,
		"FAKE_LLM_API_KEY="+fakeAPIKey,
		"FAKE_LLM_CONTROL_KEY="+fakeControlKey,
		"FAKE_LLM_MODE=accepted",
		"FAKE_LLM_TIMEOUT_MS=5000",
	), fakeLog)
	waitFixture(t, fakeURL+"/__fixture/scenarios", fakeControlKey)

	serverAddr := freeAddress(t)
	serverURL := "http://" + serverAddr
	serverEnv := append(os.Environ(),
		"DATABASE_URL="+runtimeDSN,
		"LISTEN_ADDR="+serverAddr,
		"PUBLIC_BASE_URL="+serverURL,
		"MIGRATE_ON_START=true",
		"SHUTDOWN_TIMEOUT=2s",
		"PAIRING_RATE_LIMIT_PER_MINUTE=1000",
		"AUTH_FAILURE_LIMIT_PER_MINUTE=1000",
		"DEVICE_RATE_LIMIT_PER_MINUTE=10000",
		"MODEL_REQUIRED=true",
		"MODEL_BASE_URL="+fakeURL,
		"MODEL_NAME=strict-blackbox",
		"MODEL_API_KEY="+fakeAPIKey,
		"MODEL_CONTEXT_WINDOW=8192",
		"MODEL_MIN_CONTEXT_WINDOW=4096",
		"MODEL_TIMEOUT=5s",
		"MODEL_PROBE_CACHE_TTL=100ms",
		"NOCTURNE_ENABLED=false",
	)
	if offlineSigner {
		_, offlinePrivateKey, signerErr := ed25519.GenerateKey(rand.Reader)
		if signerErr != nil {
			t.Fatalf("offline signer generation failed")
		}
		signerNow := time.Now().UTC().Truncate(time.Second)
		serverEnv = append(serverEnv,
			"OFFLINE_SIGNER_KEY_ID=blackbox-offline-signer",
			"OFFLINE_SIGNER_PRIVATE_KEY="+base64.RawURLEncoding.EncodeToString(offlinePrivateKey),
			"OFFLINE_SIGNER_ISSUED_AT="+signerNow.Add(-time.Hour).Format(time.RFC3339),
			"OFFLINE_SIGNER_NOT_AFTER="+signerNow.Add(24*time.Hour).Format(time.RFC3339),
		)
	}
	serverLog := openProcessLog(t, "edu-agentd")
	startProcess(t, "edu-agentd", serverBin, []string{"serve"}, serverEnv, serverLog)
	waitHTTPStatus(t, serverURL+"/livez", http.StatusOK, nil)

	harness := &harness{t: t, schema: schema, runtimeDSN: runtimeDSN, serverURL: serverURL, fakeURL: fakeURL, serverEnv: serverEnv, serverLogPath: serverLog.Name()}
	harness.configureFake("route", fakeScenario{Kind: "accepted", RouteStepLimit: 1})
	return harness
}

func (h *harness) ServerOffersOfflineBootstrap() bool {
	h.t.Helper()
	code := h.createPairingCode()
	body, err := json.Marshal(map[string]string{"code": code, "display_name": "offline-bootstrap-probe"})
	if err != nil {
		h.t.Fatalf("offline bootstrap probe request failed")
	}
	request, err := http.NewRequest(http.MethodPost, h.serverURL+"/v1/pairings/exchange", bytes.NewReader(body))
	if err != nil {
		h.t.Fatalf("offline bootstrap probe request failed")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		h.t.Fatalf("offline bootstrap probe failed")
	}
	defer response.Body.Close()
	var result struct {
		Offline json.RawMessage `json:"offline"`
	}
	if response.StatusCode != http.StatusCreated || json.NewDecoder(response.Body).Decode(&result) != nil {
		h.t.Fatalf("offline bootstrap probe returned an invalid response")
	}
	return len(result.Offline) > 0 && string(result.Offline) != "null"
}

func (h *harness) pairBoth(serverURL string) {
	h.t.Helper()
	h.primaryHome = h.newCLIHome("primary")
	h.secondaryHome = h.newCLIHome("secondary")
	h.pair(h.primaryHome, serverURL, "blackbox-primary")
	h.pair(h.secondaryHome, serverURL, "blackbox-secondary")
	h.primaryDevice = h.scalarString("primary device metadata", `SELECT id FROM devices WHERE display_name='blackbox-primary'`)
	h.secondDevice = h.scalarString("secondary device metadata", `SELECT id FROM devices WHERE display_name='blackbox-secondary'`)
	if h.primaryDevice == h.secondDevice || h.primaryDevice == "" || h.secondDevice == "" {
		h.t.Fatalf("pairing assertion failed: isolated device identities")
	}
}

func (h *harness) pair(home, serverURL, name string) {
	h.t.Helper()
	code := h.createPairingCode()
	result := h.runCLI(home, code+"\n", "pair", "--server", serverURL, "--name", name)
	requireExit(h.t, result, 0, "pair "+name)
	requireContains(h.t, result.stdout, "Device ID:", "pair device metadata")
}

func (h *harness) createPairingCode() string {
	h.t.Helper()
	return h.createPairingCodeForProfile("")
}

func (h *harness) createPairingCodeForProfile(profile string) string {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := []string{"pairing-code", "create"}
	if profile != "" {
		args = append(args, "--profile", profile)
	}
	cmd := exec.CommandContext(ctx, serverBin, args...)
	cmd.Env = replaceEnv(h.serverEnv, "MIGRATE_ON_START", "false")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		h.t.Fatalf("pairing setup failed: create one-time code")
	}
	code := strings.TrimSpace(stdout.String())
	if code == "" || strings.ContainsAny(code, "\r\n\t ") {
		h.t.Fatalf("pairing setup failed: invalid one-time code shape")
	}
	return code
}

type pairedCredential struct {
	DeviceID string
	Token    string
}

func (h *harness) pairCredential(profile, displayName string) pairedCredential {
	h.t.Helper()
	var response struct {
		Device struct {
			ID string `json:"id"`
		} `json:"device"`
		Token string `json:"token"`
	}
	h.authenticatedJSON(http.MethodPost, h.serverURL+"/v1/pairings/exchange", "", map[string]string{
		"code": h.createPairingCodeForProfile(profile), "display_name": displayName,
	}, http.StatusCreated, &response)
	if response.Device.ID == "" || response.Token == "" {
		h.t.Fatalf("pairing setup failed: credential response metadata")
	}
	return pairedCredential{DeviceID: response.Device.ID, Token: response.Token}
}

func (h *harness) authenticatedJSON(method, endpoint, token string, input any, wantStatus int, output any) {
	h.t.Helper()
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			h.t.Fatalf("authenticated request failed: encode input")
		}
		body = bytes.NewReader(encoded)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		h.t.Fatalf("authenticated request failed: create request")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		h.t.Fatalf("authenticated request failed: transport")
	}
	defer response.Body.Close()
	if response.StatusCode != wantStatus {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		h.t.Fatalf("authenticated request failed: method=%s status=%d want=%d body=%q", method, response.StatusCode, wantStatus, detail)
	}
	if output != nil {
		if err := json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(output); err != nil {
			h.t.Fatalf("authenticated request failed: decode response")
		}
	}
}

func (h *harness) newCLIHome(label string) string {
	h.t.Helper()
	root := filepath.Join(h.t.TempDir(), label)
	for _, dir := range []string{filepath.Join(root, "config"), filepath.Join(root, "home")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			h.t.Fatalf("CLI isolation setup failed: %s", label)
		}
	}
	return root
}

func (h *harness) runCLI(home, stdin string, args ...string) commandResult {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cliBin, args...)
	cmd.Env = append(os.Environ(),
		"HOME="+filepath.Join(home, "home"),
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"),
		"NO_COLOR=1",
		"TERM=dumb",
	)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exit := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exit = exitErr.ExitCode()
		} else {
			exit = -1
		}
	}
	return commandResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exit: exit}
}

func (h *harness) assertHomeFilesExclude(home string, values ...string) {
	h.t.Helper()
	err := filepath.WalkDir(home, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, value := range values {
			if strings.Contains(string(content), value) {
				h.t.Fatalf("CLI home plaintext exclusion failed for %s", filepath.Base(path))
			}
		}
		return nil
	})
	if err != nil {
		h.t.Fatalf("CLI home plaintext scan failed")
	}
}

func (h *harness) configureFake(kind string, scenarios ...fakeScenario) {
	h.t.Helper()
	payload := struct {
		Sequence []fakeScenario `json:"sequence"`
	}{Sequence: scenarios}
	h.controlJSON(http.MethodPut, h.fakeURL+"/__fixture/scenarios/"+kind, fakeControlKey, payload, http.StatusOK, nil)
}

func (h *harness) fakeAuditCounts() map[string]int {
	h.t.Helper()
	var response struct {
		Audit []json.RawMessage `json:"audit"`
	}
	h.controlJSON(http.MethodGet, h.fakeURL+"/__fixture/audit", fakeControlKey, nil, http.StatusOK, &response)
	counts := map[string]int{}
	for _, raw := range response.Audit {
		var entry struct {
			RequestKind string `json:"request_kind"`
			Status      int    `json:"status"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil || entry.Status < 100 || entry.Status > 599 {
			h.t.Fatalf("fixture control failed: audit entry metadata")
		}
		if entry.RequestKind != "" && entry.Status >= 200 && entry.Status < 300 {
			counts[entry.RequestKind]++
		}
	}
	return counts
}

func (h *harness) startProxy() string {
	h.t.Helper()
	proxyAddr := freeAddress(h.t)
	h.proxyURL = "http://" + proxyAddr
	proxyLog := openProcessLog(h.t, "response-loss-proxy")
	startProcess(h.t, "response-loss-proxy", proxyBin, []string{
		"-listen", proxyAddr,
		"-upstream", h.serverURL,
		"-control-key", proxyControlKey,
	}, os.Environ(), proxyLog)
	waitFixture(h.t, h.proxyURL+"/__fixture/rules", proxyControlKey)
	return h.proxyURL
}

func (h *harness) armCapture(path string) {
	h.t.Helper()
	h.controlJSON(http.MethodPost, h.proxyURL+"/__fixture/capture-next", proxyControlKey, map[string]string{
		"method": http.MethodPost,
		"path":   path,
	}, http.StatusAccepted, nil)
}

func (h *harness) proxyRules() []proxyRuleStats {
	h.t.Helper()
	var response struct {
		Rules []proxyRuleStats `json:"rules"`
	}
	h.controlJSON(http.MethodGet, h.proxyURL+"/__fixture/rules", proxyControlKey, nil, http.StatusOK, &response)
	return response.Rules
}

func (h *harness) proxyAudit() []proxyAuditEntry {
	h.t.Helper()
	var response struct {
		Audit []proxyAuditEntry `json:"audit"`
	}
	h.controlJSON(http.MethodGet, h.proxyURL+"/__fixture/audit", proxyControlKey, nil, http.StatusOK, &response)
	return response.Audit
}

func (h *harness) controlJSON(method, endpoint, key string, input any, wantStatus int, output any) {
	h.t.Helper()
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			h.t.Fatalf("fixture control failed: encode metadata")
		}
		body = bytes.NewReader(encoded)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		h.t.Fatalf("fixture control failed: create request")
	}
	request.Header.Set("X-Fixture-Control-Key", key)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		h.t.Fatalf("fixture control failed: transport")
	}
	defer response.Body.Close()
	if response.StatusCode != wantStatus {
		h.t.Fatalf("fixture control failed: status=%d want=%d", response.StatusCode, wantStatus)
	}
	if output != nil {
		decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(output); err != nil {
			h.t.Fatalf("fixture control failed: response metadata")
		}
	}
}

func (h *harness) importFixture(home string) {
	h.t.Helper()
	_, file, _, _ := runtime.Caller(0)
	fixture := filepath.Join(filepath.Dir(file), "testdata", "topic.md")
	result := h.runCLI(home, "", "knowledge", "import", fixture)
	requireExit(h.t, result, 0, "knowledge import")
	requireContains(h.t, result.stdout, "Knowledge revision:", "knowledge import metadata")
}

func (h *harness) setGoal(home, text string) string {
	h.t.Helper()
	result := h.runCLI(home, "", "goal", "set", text)
	requireExit(h.t, result, 0, "goal set")
	requireContains(h.t, result.stdout, "State: GoalReady", "goal state")
	return h.scalarString("current session metadata", `SELECT id FROM tutoring_sessions WHERE state<>'Completed' ORDER BY started_at DESC,id DESC LIMIT 1`)
}

func (h *harness) latestSession() (id, state string) {
	h.t.Helper()
	fields := h.queryFields("latest session metadata", `SELECT id,state FROM tutoring_sessions ORDER BY started_at DESC,id DESC LIMIT 1`, 2)
	return fields[0], fields[1]
}

func (h *harness) scalarInt(label, query string, args ...any) int64 {
	h.t.Helper()
	value := h.scalarString(label, query, args...)
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		h.t.Fatalf("database assertion failed: %s integer metadata", label)
	}
	return parsed
}

func (h *harness) scalarString(label, query string, args ...any) string {
	h.t.Helper()
	lines := runPSQL(h.t, h.runtimeDSN, label, expandSQL(query, args...))
	if len(lines) != 1 {
		h.t.Fatalf("database assertion failed: %s row_count=%d want=1", label, len(lines))
	}
	return lines[0]
}

func (h *harness) scalarBool(label, query string, args ...any) bool {
	h.t.Helper()
	value := h.scalarString(label, query, args...)
	if value != "t" && value != "f" {
		h.t.Fatalf("database assertion failed: %s boolean metadata", label)
	}
	return value == "t"
}

func (h *harness) queryFields(label, query string, count int, args ...any) []string {
	h.t.Helper()
	value := h.scalarString(label, query, args...)
	fields := strings.Split(value, "\t")
	if len(fields) != count {
		h.t.Fatalf("database assertion failed: %s field_count=%d want=%d", label, len(fields), count)
	}
	return fields
}

func (h *harness) ageCurrentReview() (nodeID string, step int, due time.Time) {
	h.t.Helper()
	metadata := h.scalarString("accepted evidence metadata", `
		SELECT json_build_object(
			'evidence_id',e.id::text,
			'node_revision_id',e.node_revision_id::text,
			'payload_id',p.id::text,
			'payload',p.payload)::text
		FROM learning_evidence e
		JOIN learning_events event ON event.event_type='EvidenceAccepted'
		JOIN learning_event_payloads p ON p.id=event.payload_id AND p.payload_hash=event.payload_hash
		WHERE p.payload->>'evidence_id'=e.id::text
		ORDER BY e.received_at DESC,e.id DESC LIMIT 1`)
	var record struct {
		EvidenceID     string          `json:"evidence_id"`
		NodeRevisionID string          `json:"node_revision_id"`
		PayloadID      string          `json:"payload_id"`
		Payload        json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(metadata), &record); err != nil || record.EvidenceID == "" || record.NodeRevisionID == "" || record.PayloadID == "" {
		h.t.Fatalf("review fixture failed: decode accepted evidence metadata")
	}
	nodeID = record.NodeRevisionID
	past := time.Now().UTC().Add(-25 * time.Hour).Truncate(time.Microsecond)
	var object map[string]any
	if err := json.Unmarshal(record.Payload, &object); err != nil {
		h.t.Fatalf("review fixture failed: decode event metadata")
	}
	object["received_at"] = past.Format(time.RFC3339Nano)
	encoded, err := json.Marshal(object)
	if err != nil {
		h.t.Fatalf("review fixture failed: encode event metadata")
	}
	digest := sha256.Sum256(encoded)
	newPayloadID := randomUUID(h.t)
	due = time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	fixtureSQL := fmt.Sprintf(`
		BEGIN;
		SELECT privacy_lock_owner_gate('learning','write',NULL);
		DROP TRIGGER learning_evidence_immutable ON learning_evidence;
		DROP TRIGGER learning_events_immutable ON learning_events;
		UPDATE learning_evidence SET received_at=%s::timestamptz WHERE id=%s::uuid;
		INSERT INTO learning_event_payloads(id,payload,payload_hash,created_at)
		SELECT %s::uuid,%s::jsonb,decode('%s','hex'),created_at
		FROM learning_event_payloads WHERE id=%s::uuid;
		UPDATE learning_events SET payload_id=%s::uuid,payload_hash=decode('%s','hex') WHERE payload_id=%s::uuid;
		CREATE TRIGGER learning_evidence_immutable BEFORE UPDATE OR DELETE ON learning_evidence FOR EACH ROW EXECUTE FUNCTION reject_learning_history_mutation();
		CREATE TRIGGER learning_events_immutable BEFORE UPDATE OR DELETE ON learning_events FOR EACH ROW EXECUTE FUNCTION reject_learning_history_mutation();
		COMMIT;`,
		sqlLiteral(past.Format(time.RFC3339Nano)), sqlLiteral(record.EvidenceID),
		sqlLiteral(newPayloadID), sqlLiteral(string(encoded)), hex.EncodeToString(digest[:]), sqlLiteral(record.PayloadID),
		sqlLiteral(newPayloadID), hex.EncodeToString(digest[:]), sqlLiteral(record.PayloadID),
	)
	runPSQL(h.t, h.runtimeDSN, "age review fixture", fixtureSQL)
	h.rebuildProjection()
	fields := h.queryFields("verify due review metadata", `SELECT (item->>'step')::int,extract(epoch from due_at) FROM learning_projection_reviews WHERE node_revision_id=$1`, 2, nodeID)
	parsedStep, err := strconv.Atoi(fields[0])
	if err != nil {
		h.t.Fatalf("review fixture failed: step metadata")
	}
	return nodeID, parsedStep, parseUnixTime(h.t, fields[1], "review fixture due metadata")
}

func (h *harness) rebuildProjection() {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, projectionRebuildBin)
	cmd.Env = replaceEnv(os.Environ(), "TEST_DATABASE_URL", h.runtimeDSN)
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" || len(detail) > 160 || strings.ContainsAny(detail, "\r\n\x00") {
			detail = "code=internal_error reason=invalid_fixture_diagnostic"
		}
		h.t.Fatalf("projection rebuild fixture failed: %s", detail)
	}
}

func (h *harness) sendDifferentReplayBody(path, operationID string) int {
	h.t.Helper()
	body, _ := json.Marshal(map[string]any{
		"operation_id":           operationID,
		"payload_schema_version": 1,
		"aggregate_type":         "session",
		"aggregate_id":           "00000000-0000-4000-8000-000000000000",
		"expected_version":       0,
		"action":                 "record_assessment",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.proxyURL+path, bytes.NewReader(body))
	if err != nil {
		h.t.Fatalf("response-loss assertion failed: create mismatch request")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		h.t.Fatalf("response-loss assertion failed: mismatch transport")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	return response.StatusCode
}

func requireExit(t *testing.T, result commandResult, want int, label string) {
	t.Helper()
	if result.exit != want {
		code := stableErrorCode(result.stderr)
		if code == "" {
			code = "none"
		}
		t.Fatalf("%s failed: exit=%d want=%d error_code=%s stderr=%q", label, result.exit, want, code, result.stderr)
	}
}

func requireNonZero(t *testing.T, result commandResult, label string) string {
	t.Helper()
	if result.exit == 0 {
		t.Fatalf("%s unexpectedly succeeded", label)
	}
	code := stableErrorCode(result.stderr)
	if code == "" {
		t.Fatalf("%s failed without a stable error code", label)
	}
	return code
}

func requireContains(t *testing.T, value []byte, needle, label string) {
	t.Helper()
	if !bytes.Contains(value, []byte(needle)) {
		t.Fatalf("%s missing expected stable marker", label)
	}
}

func stableErrorCode(stderr []byte) string {
	match := errorCodeRE.FindSubmatch(stderr)
	if len(match) != 2 {
		return ""
	}
	return string(match[1])
}

func startProcess(t *testing.T, name, binary string, args, env []string, logFile *os.File) *runningProcess {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("process start failed: %s", name)
	}
	process := &runningProcess{name: name, cmd: cmd, done: make(chan error, 1)}
	go func() { process.done <- cmd.Wait() }()
	t.Cleanup(func() { process.stop(t) })
	return process
}

func (p *runningProcess) stop(t *testing.T) {
	t.Helper()
	p.once.Do(func() {
		if p.cmd.Process == nil {
			return
		}
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
			return
		case <-time.After(5 * time.Second):
			_ = p.cmd.Process.Kill()
		}
		select {
		case <-p.done:
		case <-time.After(2 * time.Second):
			t.Errorf("process cleanup failed: %s", p.name)
		}
	})
}

func openProcessLog(t *testing.T, name string) *os.File {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(t.TempDir(), name+".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("process log setup failed: %s", name)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func waitFixture(t *testing.T, endpoint, key string) {
	t.Helper()
	waitHTTPStatus(t, endpoint, http.StatusOK, map[string]string{"X-Fixture-Control-Key": key})
}

func waitHTTPStatus(t *testing.T, endpoint string, want int, headers map[string]string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		response, err := http.DefaultClient.Do(request)
		cancel()
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
			if response.StatusCode == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process readiness failed: status endpoint")
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("port allocation failed")
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return address
}

func withSearchPath(t *testing.T, raw, schema string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("database setup failed: parse TEST_DATABASE_URL")
	}
	query := parsed.Query()
	query.Set("options", "-csearch_path="+schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func runPSQL(t *testing.T, dsn, label, query string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-A", "-t", "-F", "\t", dsn, "-c", query)
	cmd.Env = append(os.Environ(), "PAGER=cat", "PSQL_PAGER=cat")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		t.Fatalf("database command failed: %s", label)
	}
	value := strings.Trim(stdout.String(), "\r\n")
	if value == "" {
		return nil
	}
	lines := strings.Split(value, "\n")
	for index := range lines {
		lines[index] = strings.TrimSuffix(lines[index], "\r")
	}
	return lines
}

func runPSQLCleanup(t *testing.T, dsn, query string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", dsn, "-c", query)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		t.Errorf("database cleanup failed")
	}
}

func expandSQL(query string, args ...any) string {
	result := query
	for index := len(args); index > 0; index-- {
		result = strings.ReplaceAll(result, "$"+strconv.Itoa(index), sqlLiteral(args[index-1]))
	}
	return result
}

func sqlLiteral(value any) string {
	switch typed := value.(type) {
	case string:
		return "'" + strings.ReplaceAll(typed, "'", "''") + "'"
	case time.Time:
		return sqlLiteral(typed.UTC().Format(time.RFC3339Nano))
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case bool:
		if typed {
			return "TRUE"
		}
		return "FALSE"
	default:
		panic(fmt.Sprintf("unsupported SQL metadata type %T", value))
	}
}

func parseUnixTime(t *testing.T, value, label string) time.Time {
	t.Helper()
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil {
		t.Fatalf("database assertion failed: %s", label)
	}
	whole := int64(seconds)
	nanos := int64((seconds - float64(whole)) * float64(time.Second))
	return time.Unix(whole, nanos).UTC()
}

func replaceEnv(input []string, name, value string) []string {
	prefix := name + "="
	output := append([]string(nil), input...)
	for index, entry := range output {
		if strings.HasPrefix(entry, prefix) {
			output[index] = prefix + value
			return output
		}
	}
	return append(output, prefix+value)
}

func randomHex(t *testing.T, bytesCount int) string {
	t.Helper()
	value := make([]byte, bytesCount)
	if _, err := rand.Read(value); err != nil {
		t.Fatalf("random identifier generation failed")
	}
	return hex.EncodeToString(value)
}

func randomUUID(t *testing.T) string {
	t.Helper()
	value := randomHex(t, 16)
	return value[:8] + "-" + value[8:12] + "-4" + value[13:16] + "-8" + value[17:20] + "-" + value[20:]
}
