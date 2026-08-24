package responseproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testOperationID = "20000000-0000-4000-8000-000000000001"
	testPath        = "/v1/tutoring/sessions/10000000-0000-4000-8000-000000000001/actions"
)

func TestOneShotPostCommitResponseLossAndReplayGuard(t *testing.T) {
	var upstreamMu sync.Mutex
	var committedBody []byte
	var committedResponse []byte
	upstreamCalls := 0
	commits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		upstreamMu.Lock()
		defer upstreamMu.Unlock()
		upstreamCalls++
		if committedBody == nil {
			committedBody = append([]byte(nil), body...)
			committedResponse = []byte(`{"status":"succeeded","replayed":false}`)
			commits++
		} else if !bytes.Equal(committedBody, body) {
			t.Errorf("different body reached upstream: %s", body)
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Complete", "true")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(committedResponse)
	}))
	defer upstream.Close()

	proxy, err := New(upstream.URL, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rule := Rule{Method: http.MethodPost, Path: testPath, OperationID: testOperationID}
	if err := proxy.AddRule(rule); err != nil {
		t.Fatal(err)
	}
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	body := []byte(`{"operation_id":"` + testOperationID + `","payload_schema_version":1,"answer":"private multiline\nanswer"}`)
	response, err := postJSON(proxyServer.URL+testPath, body)
	if response != nil {
		response.Body.Close()
	}
	if err == nil || !errors.Is(err, io.EOF) {
		t.Fatalf("first request error=%T %v want EOF", err, err)
	}
	upstreamMu.Lock()
	if commits != 1 || upstreamCalls != 1 {
		t.Fatalf("after EOF commits=%d upstream_calls=%d", commits, upstreamCalls)
	}
	upstreamMu.Unlock()
	stats, ok := proxy.Stats(rule)
	if !ok || stats.Calls != 1 || stats.UpstreamCalls != 1 || stats.Drops != 1 || stats.Rejections != 0 {
		t.Fatalf("after drop stats=%+v found=%v", stats, ok)
	}

	response, err = postJSON(proxyServer.URL+testPath, body)
	if err != nil {
		t.Fatal(err)
	}
	replayedBody, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusCreated || response.Header.Get("X-Upstream-Complete") != "true" || !bytes.Equal(replayedBody, committedResponse) {
		t.Fatalf("replay status=%d header=%q body=%s err=%v", response.StatusCode, response.Header.Get("X-Upstream-Complete"), replayedBody, readErr)
	}
	upstreamMu.Lock()
	if commits != 1 || upstreamCalls != 2 {
		t.Fatalf("after replay commits=%d upstream_calls=%d", commits, upstreamCalls)
	}
	upstreamMu.Unlock()

	differentBody := []byte(`{"operation_id":"` + testOperationID + `","payload_schema_version":1,"answer":"changed private answer"}`)
	response, err = postJSON(proxyServer.URL+testPath, differentBody)
	if err != nil {
		t.Fatal(err)
	}
	mismatchBody, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusConflict || !bytes.Contains(mismatchBody, []byte("response_loss_replay_body_mismatch")) {
		t.Fatalf("mismatch status=%d body=%s err=%v", response.StatusCode, mismatchBody, readErr)
	}
	upstreamMu.Lock()
	if commits != 1 || upstreamCalls != 2 {
		t.Fatalf("mismatch reached upstream: commits=%d upstream_calls=%d", commits, upstreamCalls)
	}
	upstreamMu.Unlock()
	stats, _ = proxy.Stats(rule)
	if stats.Calls != 3 || stats.UpstreamCalls != 2 || stats.Drops != 1 || stats.Rejections != 1 {
		t.Fatalf("final stats=%+v", stats)
	}
	audit := proxy.Audit()
	if len(audit) != 3 || !audit[0].Dropped || audit[1].Dropped || !audit[2].Rejected {
		t.Fatalf("audit=%+v", audit)
	}
	auditJSON, err := json.Marshal(audit)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private multiline", "changed private answer", "succeeded", "replayed", "proxy-private-token", "proxy-private-header", "Authorization", "X-Private-Header"} {
		if strings.Contains(string(auditJSON), secret) {
			t.Fatalf("audit recorded body content %q: %s", secret, auditJSON)
		}
	}
}

func TestCaptureNextBindsRealOperationIDAndRejectsDifferentBodyBeforeUpstream(t *testing.T) {
	var upstreamMu sync.Mutex
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamMu.Lock()
		upstreamCalls++
		upstreamMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"status":"committed"}`))
	}))
	defer upstream.Close()

	proxy, err := New(upstream.URL, Options{})
	if err != nil {
		t.Fatal(err)
	}
	capture := CaptureNext{Method: http.MethodPost, Path: testPath}
	if err := proxy.ArmCaptureNext(capture); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(proxy)
	defer server.Close()

	body := []byte(`{"operation_id":"` + testOperationID + `","payload_schema_version":1}`)
	response, err := postJSON(server.URL+testPath, body)
	if response != nil {
		response.Body.Close()
	}
	if err == nil || !errors.Is(err, io.EOF) {
		t.Fatalf("captured request error=%T %v want EOF", err, err)
	}
	if captures := proxy.Captures(); len(captures) != 0 {
		t.Fatalf("capture remained armed: %+v", captures)
	}
	rules := proxy.Rules()
	if len(rules) != 1 || rules[0].Rule.OperationID != testOperationID || rules[0].Drops != 1 || rules[0].UpstreamCalls != 1 {
		t.Fatalf("captured rules=%+v", rules)
	}

	response, err = postJSON(server.URL+testPath, body)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("replay status=%d", response.StatusCode)
	}

	different := []byte(`{"operation_id":"` + testOperationID + `","payload_schema_version":1,"changed":true}`)
	response, err = postJSON(server.URL+testPath, different)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("different-body status=%d", response.StatusCode)
	}
	upstreamMu.Lock()
	calls := upstreamCalls
	upstreamMu.Unlock()
	if calls != 2 {
		t.Fatalf("different body reached upstream: calls=%d", calls)
	}
	stats, ok := proxy.Stats(Rule{Method: http.MethodPost, Path: testPath, OperationID: testOperationID})
	if !ok || stats.Calls != 3 || stats.UpstreamCalls != 2 || stats.Drops != 1 || stats.Rejections != 1 {
		t.Fatalf("capture stats=%+v found=%v", stats, ok)
	}
}

func TestCaptureNextAndExactRuleAreMutuallyExclusiveBeforeFirstRequest(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(*testing.T, *Proxy)
		body     []byte
		wantDrop bool
	}{
		{
			name: "exact rule rejects capture without stealing another operation",
			setup: func(t *testing.T, proxy *Proxy) {
				t.Helper()
				if err := proxy.AddRule(Rule{Method: http.MethodPost, Path: testPath, OperationID: testOperationID}); err != nil {
					t.Fatal(err)
				}
				if err := proxy.ArmCaptureNext(CaptureNext{Method: http.MethodPost, Path: testPath}); err == nil {
					t.Fatal("capture was armed on an exact-rule path")
				}
			},
			body: []byte(`{"operation_id":"20000000-0000-4000-8000-000000000002"}`),
		},
		{
			name: "capture rejects exact rule and binds first operation",
			setup: func(t *testing.T, proxy *Proxy) {
				t.Helper()
				if err := proxy.ArmCaptureNext(CaptureNext{Method: http.MethodPost, Path: testPath}); err != nil {
					t.Fatal(err)
				}
				if err := proxy.AddRule(Rule{Method: http.MethodPost, Path: testPath, OperationID: testOperationID}); err == nil {
					t.Fatal("exact rule was added on a capture-next path")
				}
			},
			body:     []byte(`{"operation_id":"` + testOperationID + `"}`),
			wantDrop: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstreamCalls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls++
				w.WriteHeader(http.StatusCreated)
			}))
			defer upstream.Close()
			proxy, err := New(upstream.URL, Options{})
			if err != nil {
				t.Fatal(err)
			}
			test.setup(t, proxy)
			server := httptest.NewServer(proxy)
			defer server.Close()

			response, requestErr := postJSON(server.URL+testPath, test.body)
			if response != nil {
				response.Body.Close()
			}
			if !test.wantDrop {
				if requestErr != nil || response == nil || response.StatusCode != http.StatusCreated || upstreamCalls != 1 {
					t.Fatalf("unconfigured first request error=%v response=%v upstream=%d", requestErr, response != nil, upstreamCalls)
				}
				stats, ok := proxy.Stats(Rule{Method: http.MethodPost, Path: testPath, OperationID: testOperationID})
				if !ok || stats.Calls != 0 || stats.UpstreamCalls != 0 || stats.Drops != 0 {
					t.Fatalf("exact rule consumed another operation: stats=%+v found=%v", stats, ok)
				}
				return
			}
			if requestErr == nil || !errors.Is(requestErr, io.EOF) || upstreamCalls != 1 {
				t.Fatalf("captured first request error=%v upstream=%d", requestErr, upstreamCalls)
			}
			rules := proxy.Rules()
			if len(rules) != 1 || rules[0].Rule.OperationID != testOperationID || rules[0].Drops != 1 {
				t.Fatalf("captured first request rules=%+v", rules)
			}
		})
	}
}

func TestResetExcludesBlockedInflightRequestFromNewGeneration(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var upstreamMu sync.Mutex
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamMu.Lock()
		upstreamCalls++
		upstreamMu.Unlock()
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"status":"committed"}`))
	}))
	defer upstream.Close()

	proxy, err := New(upstream.URL, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rule := Rule{Method: http.MethodPost, Path: testPath, OperationID: testOperationID}
	if err := proxy.AddRule(rule); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	body := []byte(`{"operation_id":"` + testOperationID + `","payload_schema_version":1}`)
	oldResult := make(chan error, 1)
	go func() {
		response, requestErr := postJSON(server.URL+testPath, body)
		if response != nil {
			response.Body.Close()
		}
		oldResult <- requestErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for blocked upstream")
	}

	proxy.Reset()
	if err := proxy.AddRule(rule); err != nil {
		t.Fatal(err)
	}
	if stats, ok := proxy.Stats(rule); !ok || stats.Calls != 0 || stats.UpstreamCalls != 0 || stats.Drops != 0 {
		t.Fatalf("new rule inherited old request state: stats=%+v found=%v", stats, ok)
	}
	if audit := proxy.Audit(); len(audit) != 0 {
		t.Fatalf("reset did not clear audit: %+v", audit)
	}
	close(release)
	select {
	case requestErr := <-oldResult:
		if requestErr == nil {
			t.Fatal("old configured request unexpectedly retained a response")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for old request completion")
	}
	if audit := proxy.Audit(); len(audit) != 0 {
		t.Fatalf("old generation repopulated audit: %+v", audit)
	}
	if stats, _ := proxy.Stats(rule); stats.Calls != 0 || stats.UpstreamCalls != 0 || stats.Drops != 0 {
		t.Fatalf("old request consumed new rule: %+v", stats)
	}

	response, err := postJSON(server.URL+testPath, body)
	if response != nil {
		response.Body.Close()
	}
	if err == nil || !errors.Is(err, io.EOF) {
		t.Fatalf("new rule did not drop its first response: error=%T %v", err, err)
	}
	stats, ok := proxy.Stats(rule)
	if !ok || stats.Calls != 1 || stats.UpstreamCalls != 1 || stats.Drops != 1 {
		t.Fatalf("new rule stats=%+v found=%v", stats, ok)
	}
	audit := proxy.Audit()
	if len(audit) != 1 || audit[0].Sequence != 1 || !audit[0].Dropped {
		t.Fatalf("new generation audit=%+v", audit)
	}
	upstreamMu.Lock()
	defer upstreamMu.Unlock()
	if upstreamCalls != 2 {
		t.Fatalf("upstream calls=%d want=2", upstreamCalls)
	}
}

func TestRuleMatchesExactMethodPathAndOperationID(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	proxy, err := New(upstream.URL, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := proxy.AddRule(Rule{Method: http.MethodPost, Path: testPath, OperationID: testOperationID}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(proxy)
	defer server.Close()

	otherOperation := "20000000-0000-4000-8000-000000000002"
	response, err := postJSON(server.URL+testPath, []byte(`{"operation_id":"`+otherOperation+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || calls != 1 {
		t.Fatalf("status=%d calls=%d", response.StatusCode, calls)
	}
}

func TestControlAPIConfiguresAndQueriesAudit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer upstream.Close()
	proxy, err := New(upstream.URL, Options{ControlKey: "control"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(proxy)
	defer server.Close()

	captureBody, _ := json.Marshal(CaptureNext{Method: http.MethodPost, Path: testPath})
	request, _ := http.NewRequest(http.MethodPost, server.URL+ControlPrefix+"/capture-next", bytes.NewReader(captureBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Fixture-Control-Key", "control")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("arm capture status=%d", response.StatusCode)
	}

	ruleBody, _ := json.Marshal(Rule{Method: http.MethodPost, Path: testPath, OperationID: testOperationID})
	request, _ = http.NewRequest(http.MethodPost, server.URL+ControlPrefix+"/rules", bytes.NewReader(ruleBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Fixture-Control-Key", "control")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("capture path accepted exact rule status=%d", response.StatusCode)
	}
	request, _ = http.NewRequest(http.MethodPost, server.URL+ControlPrefix+"/reset", nil)
	request.Header.Set("X-Fixture-Control-Key", "control")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("reset status=%d", response.StatusCode)
	}
	ruleBody, _ = json.Marshal(Rule{Method: http.MethodPost, Path: testPath, OperationID: testOperationID})
	request, _ = http.NewRequest(http.MethodPost, server.URL+ControlPrefix+"/rules", bytes.NewReader(ruleBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Fixture-Control-Key", "control")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create rule after reset status=%d", response.StatusCode)
	}
	request, _ = http.NewRequest(http.MethodGet, server.URL+ControlPrefix+"/rules", nil)
	request.Header.Set("X-Fixture-Control-Key", "control")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result struct {
		Rules []RuleStats `json:"rules"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil || len(result.Rules) != 1 {
		t.Fatalf("rules=%+v err=%v", result.Rules, err)
	}
}

func postJSON(rawURL string, body []byte) (*http.Response, error) {
	request, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer proxy-private-token")
	request.Header.Set("X-Private-Header", "proxy-private-header")
	return http.DefaultClient.Do(request)
}
