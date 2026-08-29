package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/credentials"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/dashboard"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/terminal"
)

const (
	testDeviceID = "10000000-0000-4000-8000-000000000001"
	testDocID    = "20000000-0000-4000-8000-000000000001"
	testDocRevID = "30000000-0000-4000-8000-000000000001"
)

type shortWriter struct{}

func (shortWriter) Write(value []byte) (int, error) {
	if len(value) == 0 {
		return 0, nil
	}
	return len(value) - 1, nil
}

type memoryConfigStore struct {
	value            config.Config
	present          bool
	saveErr          error
	deleteErr        error
	deleteCalls      int
	journal          config.PairingJournal
	journalPresent   bool
	journalSaveErr   error
	journalDeleteErr error
	savePublishes    bool
	saveCalls        int
}

func (s *memoryConfigStore) Load() (config.Config, error) {
	if !s.present {
		return config.Config{}, config.ErrNotFound
	}
	return s.value, nil
}
func (s *memoryConfigStore) Save(value config.Config) error {
	s.saveCalls++
	if s.savePublishes {
		s.value, s.present = value, true
	}
	if s.saveErr != nil {
		return s.saveErr
	}
	s.value, s.present = value, true
	return nil
}
func (s *memoryConfigStore) Delete() error {
	s.deleteCalls++
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.present = false
	return nil
}
func (s *memoryConfigStore) LoadPairingJournal() (config.PairingJournal, error) {
	if !s.journalPresent {
		return config.PairingJournal{}, config.ErrJournalNotFound
	}
	return s.journal, nil
}
func (s *memoryConfigStore) SavePairingJournal(value config.PairingJournal) error {
	if s.journalSaveErr != nil {
		return s.journalSaveErr
	}
	value.SchemaVersion = 1
	s.journal, s.journalPresent = value, true
	return nil
}
func (s *memoryConfigStore) DeletePairingJournal() error {
	if s.journalDeleteErr != nil {
		return s.journalDeleteErr
	}
	s.journalPresent = false
	return nil
}

type memoryCredentialStore struct {
	record      credentials.Record
	present     bool
	saveErr     error
	deleteErr   error
	deleteCalls int
	saveCalls   int
}

func (s *memoryCredentialStore) Load() (credentials.Record, error) {
	if !s.present {
		return credentials.Record{}, credentials.ErrNotFound
	}
	return s.record, nil
}
func (s *memoryCredentialStore) Save(record credentials.Record) error {
	s.saveCalls++
	if s.saveErr != nil {
		return s.saveErr
	}
	s.record, s.present = record, true
	return nil
}
func (s *memoryCredentialStore) Delete() error {
	s.deleteCalls++
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.present = false
	return nil
}

type fakeDashboardResult struct {
	args     []string
	modelKey string
	quit     bool
	err      error
}

type fakeDashboard struct {
	results         []fakeDashboardResult
	snapshots       []dashboard.Snapshot
	pendingModelKey string
}

func (d *fakeDashboard) Run(_ context.Context, snapshot dashboard.Snapshot) ([]string, bool, error) {
	d.snapshots = append(d.snapshots, snapshot)
	if len(d.results) == 0 {
		return nil, true, nil
	}
	result := d.results[0]
	d.results = d.results[1:]
	d.pendingModelKey = result.modelKey
	return result.args, result.quit, result.err
}

func (d *fakeDashboard) TakeModelKey() (string, bool) {
	if d.pendingModelKey == "" {
		return "", false
	}
	value := d.pendingModelKey
	d.pendingModelKey = ""
	return value, true
}

type fakeTerminal struct {
	secret        string
	secretCalls   int
	lines         []string
	readLineCalls int
	confirmed     bool
	confirmCalls  int
	clearErr      error
	clearCalls    int
}

func (t *fakeTerminal) ReadSecret(string) (string, error) {
	t.secretCalls++
	return t.secret, nil
}
func (t *fakeTerminal) ReadLine(string) (string, error) {
	t.readLineCalls++
	if len(t.lines) == 0 {
		return "", io.EOF
	}
	value := t.lines[0]
	t.lines = t.lines[1:]
	return value, nil
}
func (t *fakeTerminal) Confirm(string) (bool, error) {
	t.confirmCalls++
	return t.confirmed, nil
}
func (t *fakeTerminal) Clear() error {
	t.clearCalls++
	return t.clearErr
}

type blockingReadTerminal struct {
	fakeTerminal
	started  chan struct{}
	release  chan struct{}
	returned chan struct{}
}

func (t *blockingReadTerminal) ReadLine(string) (string, error) {
	close(t.started)
	<-t.release
	close(t.returned)
	return "", nil
}

func TestNoArgsUsesDashboardOnlyForInteractiveTTY(t *testing.T) {
	t.Parallel()
	t.Run("non tty preserves usage error", func(t *testing.T) {
		dashboardRunner := &fakeDashboard{}
		app, _, errOut := newTestApp(&memoryConfigStore{}, &memoryCredentialStore{}, &fakeTerminal{})
		app.Dashboard = dashboardRunner
		app.InputIsTTY = func() bool { return true }
		app.OutputIsTTY = func() bool { return false }
		if exit := app.Run(t.Context(), nil); exit != ExitInput || !strings.Contains(errOut.String(), "error[usage]: a command is required") {
			t.Fatalf("exit=%d err=%q", exit, errOut.String())
		}
		if len(dashboardRunner.snapshots) != 0 {
			t.Fatal("dashboard ran for non-TTY output")
		}
	})

	t.Run("dumb terminal preserves usage error", func(t *testing.T) {
		dashboardRunner := &fakeDashboard{}
		app, _, errOut := newTestApp(&memoryConfigStore{}, &memoryCredentialStore{}, &fakeTerminal{})
		app.Dashboard = dashboardRunner
		app.InputIsTTY = func() bool { return true }
		app.OutputIsTTY = func() bool { return true }
		app.Getenv = func(name string) string {
			if name == "TERM" {
				return "dumb"
			}
			return ""
		}
		if exit := app.Run(t.Context(), nil); exit != ExitInput || !strings.Contains(errOut.String(), "error[usage]: a command is required") {
			t.Fatalf("exit=%d err=%q", exit, errOut.String())
		}
		if len(dashboardRunner.snapshots) != 0 {
			t.Fatal("dashboard ran for TERM=dumb")
		}
	})

	t.Run("tty dispatches existing command and returns", func(t *testing.T) {
		configStore, credentialStore := pairedStores(config.DefaultServerURL, "never-render-this-token")
		dashboardRunner := &fakeDashboard{results: []fakeDashboardResult{{args: []string{"version"}}, {quit: true}}}
		app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{lines: []string{""}})
		app.Dashboard = dashboardRunner
		app.InputIsTTY = func() bool { return true }
		app.OutputIsTTY = func() bool { return true }
		app.Build = BuildInfo{Version: "v9.8.7", Commit: "dashboard-test"}
		if exit := app.Run(t.Context(), nil); exit != ExitOK {
			t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
		}
		if !strings.Contains(out.String(), "v9.8.7") || len(dashboardRunner.snapshots) != 2 {
			t.Fatalf("out=%q snapshots=%#v", out.String(), dashboardRunner.snapshots)
		}
		for _, snapshot := range dashboardRunner.snapshots {
			if snapshot.LocalState != dashboard.LocalStatePaired || snapshot.ServerURL != config.DefaultServerURL || snapshot.DeviceName != "Laptop" {
				t.Fatalf("snapshot=%+v", snapshot)
			}
		}
	})
}

func TestDashboardSnapshotClassifiesIncompleteBinding(t *testing.T) {
	t.Parallel()
	configStore, _ := pairedStores(config.DefaultServerURL, "never-render-this-token")
	app, _, _ := newTestApp(configStore, &memoryCredentialStore{}, &fakeTerminal{})
	if snapshot := app.dashboardSnapshot(); snapshot.LocalState != dashboard.LocalStateIncomplete {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}

func TestDashboardCancellationSkipsReturnPrompt(t *testing.T) {
	t.Parallel()
	terminal := &fakeTerminal{lines: []string{"must remain unread"}}
	dashboardRunner := &fakeDashboard{results: []fakeDashboardResult{{args: []string{"version"}}}}
	app, _, _ := newTestApp(&memoryConfigStore{}, &memoryCredentialStore{}, terminal)
	app.Dashboard = dashboardRunner
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if exit := app.Run(ctx, nil); exit != ExitOK {
		t.Fatalf("exit=%d", exit)
	}
	if terminal.readLineCalls != 0 {
		t.Fatalf("read line calls=%d", terminal.readLineCalls)
	}
}

func TestDashboardCancellationInterruptsReturnPrompt(t *testing.T) {
	t.Parallel()
	terminal := &blockingReadTerminal{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		returned: make(chan struct{}),
	}
	dashboardRunner := &fakeDashboard{results: []fakeDashboardResult{{args: []string{"version"}}}}
	app, _, _ := newTestApp(&memoryConfigStore{}, &memoryCredentialStore{}, terminal)
	app.Dashboard = dashboardRunner
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }
	ctx, cancel := context.WithCancel(t.Context())
	exitResult := make(chan int, 1)
	go func() { exitResult <- app.Run(ctx, nil) }()

	select {
	case <-terminal.started:
	case <-time.After(time.Second):
		t.Fatal("return prompt did not start")
	}
	cancel()
	select {
	case exit := <-exitResult:
		close(terminal.release)
		<-terminal.returned
		if exit != ExitOK {
			t.Fatalf("exit=%d", exit)
		}
	case <-time.After(time.Second):
		close(terminal.release)
		<-terminal.returned
		t.Fatal("dashboard did not return after context cancellation")
	}
}

func TestDashboardReturnsLastCommandFailure(t *testing.T) {
	t.Parallel()
	dashboardRunner := &fakeDashboard{results: []fakeDashboardResult{{args: []string{"not-a-command"}}, {quit: true}}}
	app, _, errOut := newTestApp(&memoryConfigStore{}, &memoryCredentialStore{}, &fakeTerminal{lines: []string{""}})
	app.Dashboard = dashboardRunner
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }
	if exit := app.Run(t.Context(), nil); exit != ExitInput {
		t.Fatalf("exit=%d err=%q", exit, errOut.String())
	}
	if !strings.Contains(errOut.String(), "错误[usage]") || !strings.Contains(errOut.String(), "输入的操作或参数不符合要求") || strings.Contains(errOut.String(), "unknown command") {
		t.Fatalf("dashboard error was not localized: %q", errOut.String())
	}
}

func TestDashboardLocalizesCommandOutputWithoutChangingExplicitOutput(t *testing.T) {
	t.Parallel()
	value := config.Config{Timeout: "45s", Color: "auto"}
	configStore := &memoryConfigStore{present: true, value: value}
	dashboardRunner := &fakeDashboard{results: []fakeDashboardResult{{args: []string{"config", "show"}}, {quit: true}}}
	app, out, errOut := newTestApp(configStore, &memoryCredentialStore{}, &fakeTerminal{lines: []string{""}})
	app.Dashboard = dashboardRunner
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }
	if exit := app.Run(t.Context(), nil); exit != ExitOK {
		t.Fatalf("dashboard exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "服务器：") || !strings.Contains(out.String(), "请求超时：45s") || !strings.Contains(out.String(), "输出颜色：auto") || strings.Contains(out.String(), "Server:") {
		t.Fatalf("dashboard output was not localized: %q", out.String())
	}

	explicitApp, explicitOut, explicitErr := newTestApp(configStore, &memoryCredentialStore{}, &fakeTerminal{})
	if exit := explicitApp.Run(t.Context(), []string{"config", "show"}); exit != ExitOK {
		t.Fatalf("explicit exit=%d out=%q err=%q", exit, explicitOut.String(), explicitErr.String())
	}
	if !strings.Contains(explicitOut.String(), "Server:") || !strings.Contains(explicitOut.String(), "Timeout: 45s") || strings.Contains(explicitOut.String(), "服务器：") {
		t.Fatalf("explicit output contract changed: %q", explicitOut.String())
	}
}

func TestDashboardTextAndLearningLabelsAreModeScoped(t *testing.T) {
	t.Parallel()
	app := &App{}
	if got := app.dashboardText("Pairing code: ", "配对码："); got != "Pairing code: " {
		t.Fatalf("explicit prompt = %q", got)
	}
	app.dashboardMode = true
	if got := app.dashboardText("Pairing code: ", "配对码："); got != "配对码：" {
		t.Fatalf("dashboard prompt = %q", got)
	}

	got := localizeDashboardOutput("Allowed help: none,hint\nCommands: :ask :quit\nCurrent: answer (not scored)\n")
	for _, want := range []string{"可选帮助等级：none,hint", "可用命令：:ask :quit", "当前：回答（不计分）"} {
		if !strings.Contains(got, want) {
			t.Fatalf("localized learning output = %q, missing %q", got, want)
		}
	}
}

func TestDashboardOutputWriterRejectsShortWrite(t *testing.T) {
	t.Parallel()
	written, err := (dashboardOutputWriter{target: shortWriter{}}).Write([]byte("Server: http://127.0.0.1\n"))
	if written != 0 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("write = (%d, %v), want (0, io.ErrShortWrite)", written, err)
	}
}

func TestExplicitCommandBypassesDashboard(t *testing.T) {
	t.Parallel()
	dashboardRunner := &fakeDashboard{}
	app, out, _ := newTestApp(&memoryConfigStore{}, &memoryCredentialStore{}, &fakeTerminal{})
	app.Dashboard = dashboardRunner
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }
	if exit := app.Run(t.Context(), []string{"version"}); exit != ExitOK || !strings.Contains(out.String(), "edu-agent") {
		t.Fatalf("exit=%d out=%q", exit, out.String())
	}
	if len(dashboardRunner.snapshots) != 0 {
		t.Fatal("explicit command entered dashboard")
	}
}

func TestClientConfigUpdatesOnlySafePreferences(t *testing.T) {
	t.Parallel()
	token := "never-render-this-token"
	configStore, credentialStore := pairedStores(config.DefaultServerURL, token)
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	if exit := app.Run(t.Context(), []string{"config", "set", "--timeout", "45s", "--color", "auto"}); exit != ExitOK {
		t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if configStore.value.Timeout != "45s" || configStore.value.Color != "auto" || configStore.value.ServerURL != config.DefaultServerURL {
		t.Fatalf("config=%+v", configStore.value)
	}
	if credentialStore.record.Token != token || strings.Contains(out.String()+errOut.String(), token) {
		t.Fatal("credential changed or token was rendered")
	}

	out.Reset()
	if exit := app.Run(t.Context(), []string{"config", "show"}); exit != ExitOK || !strings.Contains(out.String(), "Timeout: 45s") || !strings.Contains(out.String(), "Color: auto") {
		t.Fatalf("show exit=%d out=%q", exit, out.String())
	}
}

func TestClientConfigCanBeSetBeforePairing(t *testing.T) {
	t.Parallel()
	configStore := &memoryConfigStore{}
	app, out, errOut := newTestApp(configStore, &memoryCredentialStore{}, &fakeTerminal{})
	if exit := app.Run(t.Context(), []string{"config", "set", "--timeout", "45s", "--color", "auto"}); exit != ExitOK {
		t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if !configStore.present || configStore.value.HasPairingBinding() || configStore.value.Timeout != "45s" || configStore.value.Color != "auto" {
		t.Fatalf("config=%+v", configStore.value)
	}
}

func TestUnpairedClientSettingsAreNotReportedAsOrphaned(t *testing.T) {
	t.Parallel()
	preset := config.DefaultAgentConfig("ollama")
	configStore := &memoryConfigStore{present: true, value: config.Config{Timeout: "45s", Color: "auto", Agent: &preset}}
	app, _, _ := newTestApp(configStore, &memoryCredentialStore{}, &fakeTerminal{})

	_, _, bindingErr := app.loadBinding(config.Overrides{})
	var commandErr *Error
	if !errors.As(bindingErr, &commandErr) || commandErr.Code != "not_paired" {
		t.Fatalf("loadBinding error = %v, want not_paired", bindingErr)
	}
	_, mutableErr := app.loadMutableClientConfig()
	if !errors.As(mutableErr, &commandErr) || commandErr.Code != "not_paired" {
		t.Fatalf("loadMutableClientConfig error = %v, want not_paired", mutableErr)
	}
}

func TestClientConfigRejectsInvalidPreferenceWithoutSaving(t *testing.T) {
	t.Parallel()
	configStore, credentialStore := pairedStores(config.DefaultServerURL, "token")
	app, _, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	if exit := app.Run(t.Context(), []string{"config", "set", "--color", "rainbow"}); exit != ExitInput {
		t.Fatalf("exit=%d err=%q", exit, errOut.String())
	}
	if configStore.saveCalls != 0 || configStore.value.Color != "never" {
		t.Fatalf("save calls=%d config=%+v", configStore.saveCalls, configStore.value)
	}
}

func TestAppVersionAndPairRejectsSecretFlag(t *testing.T) {
	t.Parallel()
	configStore := &memoryConfigStore{}
	credentialStore := &memoryCredentialStore{}
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	app.Build = BuildInfo{Version: "v1.2.3", Commit: "abc123"}
	if exit := app.Run(t.Context(), []string{"version"}); exit != ExitOK || !strings.Contains(out.String(), "v1.2.3") {
		t.Fatalf("version exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	out.Reset()
	secretFlagValue := "top-secret-flag-value-123"
	if exit := app.Run(t.Context(), []string{"pair", "--code", secretFlagValue}); exit != ExitInput {
		t.Fatalf("pair --code exit=%d", exit)
	}
	if strings.Contains(out.String()+errOut.String(), secretFlagValue) {
		t.Fatal("secret flag value appeared in output")
	}
}

func TestPairPersistsCredentialBeforeConfigWithoutSecretOutput(t *testing.T) {
	t.Parallel()
	pairCode := "pairing-code-secret"
	token := "device-token-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pairings/exchange" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var request map[string]string
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["code"] != pairCode || request["display_name"] != "Laptop" {
			t.Fatalf("request = %#v", request)
		}
		writeJSONTest(w, http.StatusCreated, api.IssuedCredential{Device: testDevice("Laptop"), Token: token})
	}))
	defer server.Close()
	configStore := &memoryConfigStore{}
	credentialStore := &memoryCredentialStore{}
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{secret: pairCode})
	if exit := app.Run(t.Context(), []string{"pair", "--server", server.URL, "--name", "Laptop"}); exit != ExitOK {
		t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if !configStore.present || !credentialStore.present || credentialStore.record.Token != token || configStore.value.DeviceID != testDeviceID {
		t.Fatalf("config=%+v credential=%+v", configStore, credentialStore)
	}
	combined := out.String() + errOut.String()
	if strings.Contains(combined, pairCode) || strings.Contains(combined, token) {
		t.Fatalf("secret appeared in output %q", combined)
	}
}

func TestPairReportsUnknownResultForGatewayLoss(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "proxy")
	}))
	defer server.Close()
	app, _, errOut := newTestApp(&memoryConfigStore{}, &memoryCredentialStore{}, &fakeTerminal{secret: "one-time-code"})
	if exit := app.Run(t.Context(), []string{"pair", "--server", server.URL, "--name", "Laptop"}); exit != ExitUnavailable || !strings.Contains(errOut.String(), "pairing_result_unknown") {
		t.Fatalf("exit=%d err=%q", exit, errOut.String())
	}
}

func TestPersistPairingCompensatesAndFailsClosed(t *testing.T) {
	t.Parallel()
	value := config.Config{ServerURL: config.DefaultServerURL, DeviceID: testDeviceID, DisplayName: "Laptop", Timeout: "30s", Color: "never"}
	record := credentials.Record{ServerURL: config.DefaultServerURL, DeviceID: testDeviceID, Token: "token"}
	t.Run("credential failure publishes no config", func(t *testing.T) {
		configStore := &memoryConfigStore{}
		credentialStore := &memoryCredentialStore{saveErr: errors.New("credential write failed")}
		err := persistPairing(configStore, credentialStore, value, record)
		var commandErr *Error
		if !errors.As(err, &commandErr) || commandErr.Code != "credential_save_failed" || configStore.present || credentialStore.present {
			t.Fatalf("err=%v config=%+v credential=%+v", err, configStore, credentialStore)
		}
	})
	t.Run("config failure removes credential", func(t *testing.T) {
		configStore := &memoryConfigStore{saveErr: errors.New("rename failed")}
		credentialStore := &memoryCredentialStore{}
		err := persistPairing(configStore, credentialStore, value, record)
		var commandErr *Error
		if !errors.As(err, &commandErr) || commandErr.Code != "config_save_failed" || credentialStore.present || credentialStore.deleteCalls != 1 {
			t.Fatalf("err=%v credential=%+v", err, credentialStore)
		}
	})
	t.Run("compensation failure leaves pending journal", func(t *testing.T) {
		configStore := &memoryConfigStore{saveErr: errors.New("rename failed")}
		credentialStore := &memoryCredentialStore{deleteErr: errors.New("delete failed")}
		err := persistPairing(configStore, credentialStore, value, record)
		var commandErr *Error
		if !errors.As(err, &commandErr) || commandErr.Code != "local_state_pending" || !credentialStore.present || !configStore.journalPresent {
			t.Fatalf("err=%v config=%+v credential=%+v", err, configStore, credentialStore)
		}
	})
	t.Run("post-rename sync and compensation failures cannot leave a login binding", func(t *testing.T) {
		configStore := &memoryConfigStore{saveErr: errors.New("directory fsync failed"), savePublishes: true, deleteErr: errors.New("config cleanup failed")}
		credentialStore := &memoryCredentialStore{deleteErr: errors.New("credential cleanup failed")}
		err := persistPairing(configStore, credentialStore, value, record)
		var commandErr *Error
		if !errors.As(err, &commandErr) || commandErr.Code != "local_state_pending" || !configStore.present || !credentialStore.present || !configStore.journalPresent {
			t.Fatalf("err=%v config=%+v credential=%+v", err, configStore, credentialStore)
		}
		app, _, _ := newTestApp(configStore, credentialStore, &fakeTerminal{})
		app.NewClient = func(string, string, time.Duration) APIClient {
			panic("pending binding must not create a network client")
		}
		if _, _, loadErr := app.loadBinding(config.Overrides{}); !errors.As(loadErr, &commandErr) || commandErr.Code != "local_state_pending" {
			t.Fatalf("loadBinding error = %v", loadErr)
		}
	})
	t.Run("journal clear failure blocks a complete binding", func(t *testing.T) {
		configStore := &memoryConfigStore{journalDeleteErr: errors.New("journal fsync failed")}
		credentialStore := &memoryCredentialStore{}
		err := persistPairing(configStore, credentialStore, value, record)
		var commandErr *Error
		if !errors.As(err, &commandErr) || commandErr.Code != "local_state_pending" || !configStore.present || !credentialStore.present || !configStore.journalPresent {
			t.Fatalf("err=%v config=%+v credential=%+v", err, configStore, credentialStore)
		}
	})
}

func TestEnvironmentTokenCannotMaskOrphanOrBindingMismatch(t *testing.T) {
	t.Parallel()
	configStore := &memoryConfigStore{present: true, value: config.Config{ServerURL: config.DefaultServerURL, DeviceID: testDeviceID, DisplayName: "Laptop", Timeout: "30s", Color: "never"}}
	app, _, _ := newTestApp(configStore, &memoryCredentialStore{}, &fakeTerminal{})
	values := map[string]string{
		"EDU_AGENT_TOKEN":           "environment-token",
		"EDU_AGENT_TOKEN_SERVER":    config.DefaultServerURL,
		"EDU_AGENT_TOKEN_DEVICE_ID": testDeviceID,
	}
	app.Getenv = func(name string) string { return values[name] }
	_, _, err := app.loadBinding(config.Overrides{})
	var commandErr *Error
	if !errors.As(err, &commandErr) || commandErr.Code != "local_state_orphaned" {
		t.Fatalf("missing credential error = %v", err)
	}

	configStore, credentialStore := pairedStores(config.DefaultServerURL, "stored-token")
	app, _, _ = newTestApp(configStore, credentialStore, &fakeTerminal{})
	app.Getenv = func(name string) string { return values[name] }
	bound, _, err := app.loadBinding(config.Overrides{})
	if err != nil || bound.Token != "environment-token" {
		t.Fatalf("bound=%+v err=%v", bound, err)
	}
	values["EDU_AGENT_TOKEN_DEVICE_ID"] = "different-device"
	_, _, err = app.loadBinding(config.Overrides{})
	if !errors.As(err, &commandErr) || commandErr.Code != "environment_token_binding_mismatch" {
		t.Fatalf("mismatch error = %v", err)
	}
	values["EDU_AGENT_TOKEN_DEVICE_ID"] = ""
	_, _, err = app.loadBinding(config.Overrides{})
	if !errors.As(err, &commandErr) || commandErr.Code != "environment_token_binding_required" {
		t.Fatalf("missing binding error = %v", err)
	}
}

func TestLoadBindingFailsClosedForOrphanAndMismatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		config     *memoryConfigStore
		credential *memoryCredentialStore
		wantCode   string
	}{
		{
			name:       "orphan credential",
			config:     &memoryConfigStore{},
			credential: &memoryCredentialStore{present: true, record: credentials.Record{ServerURL: config.DefaultServerURL, DeviceID: testDeviceID, Token: "token"}},
			wantCode:   "local_state_orphaned",
		},
		{
			name:       "missing credential",
			config:     &memoryConfigStore{present: true, value: config.Config{ServerURL: config.DefaultServerURL, DeviceID: testDeviceID, DisplayName: "Laptop", Timeout: "30s", Color: "never"}},
			credential: &memoryCredentialStore{},
			wantCode:   "local_state_orphaned",
		},
		{
			name:       "device mismatch",
			config:     &memoryConfigStore{present: true, value: config.Config{ServerURL: config.DefaultServerURL, DeviceID: testDeviceID, DisplayName: "Laptop", Timeout: "30s", Color: "never"}},
			credential: &memoryCredentialStore{present: true, record: credentials.Record{ServerURL: config.DefaultServerURL, DeviceID: "different-device", Token: "token"}},
			wantCode:   "device_mismatch",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, _, _ := newTestApp(test.config, test.credential, &fakeTerminal{})
			_, _, err := app.loadBinding(config.Overrides{})
			var commandErr *Error
			if !errors.As(err, &commandErr) || commandErr.Code != test.wantCode {
				t.Fatalf("error = %v, want %s", err, test.wantCode)
			}
		})
	}
}

func TestDeviceStatusLogoutAndForgetLocal(t *testing.T) {
	t.Parallel()
	token := "device-token-secret"
	var revokeStatus atomic.Int32
	revokeStatus.Store(http.StatusInternalServerError)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" && r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("authorization missing for %s", r.URL.Path)
		}
		switch r.URL.Path {
		case "/v1/devices":
			writeJSONTest(w, http.StatusOK, api.DevicesResponse{Devices: []api.Device{testDevice("Laptop")}})
		case "/readyz":
			writeJSONTest(w, http.StatusOK, api.Readiness{Status: "degraded", Components: map[string]api.HealthComponent{"database": {Status: "healthy"}, "model": {Status: "degraded", Reason: "not_configured"}}})
		case "/v1/model/capabilities":
			writeJSONTest(w, http.StatusOK, api.ModelCapabilities{Profile: "openai-chat-completions-v1", Compatible: false, IncompatibilityReasons: []string{"not_configured"}})
		case "/v1/devices/" + testDeviceID:
			if status := int(revokeStatus.Load()); status != http.StatusNoContent {
				writeJSONTest(w, status, api.ErrorResponse{Error: api.ErrorBody{Code: "internal_error", Message: "failed", RequestID: "request-logout"}})
			} else {
				w.WriteHeader(http.StatusNoContent)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	configStore, credentialStore := pairedStores(server.URL, token)
	modelPreset := config.DefaultAgentConfig("deepseek")
	configStore.value.Agent = &modelPreset
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{confirmed: true})
	if exit := app.Run(t.Context(), []string{"device", "status"}); exit != ExitOK {
		t.Fatalf("status exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if strings.Contains(out.String()+errOut.String(), token) || !strings.Contains(out.String(), "Readiness: degraded") || !strings.Contains(out.String(), "Scopes: devices:manage, devices:read, model:probe") {
		t.Fatalf("unsafe or incomplete status output %q %q", out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if exit := app.Run(t.Context(), []string{"logout"}); exit != ExitInternal || !configStore.present || !credentialStore.present {
		t.Fatalf("failed logout exit=%d config=%+v credential=%+v", exit, configStore, credentialStore)
	}
	revokeStatus.Store(http.StatusNoContent)
	out.Reset()
	errOut.Reset()
	if exit := app.Run(t.Context(), []string{"logout"}); exit != ExitOK || !configStore.present || configStore.value.HasPairingBinding() || configStore.value.Agent == nil || credentialStore.present {
		t.Fatalf("successful logout exit=%d config=%+v credential=%+v out=%q err=%q", exit, configStore, credentialStore, out.String(), errOut.String())
	}

	configStore, credentialStore = pairedStores(server.URL, token)
	configStore.value.Agent = &modelPreset
	app, out, errOut = newTestApp(configStore, credentialStore, &fakeTerminal{confirmed: true})
	app.NewClient = func(string, string, time.Duration) APIClient { panic("forget-local must not create a network client") }
	if exit := app.Run(t.Context(), []string{"device", "forget-local"}); exit != ExitOK || !configStore.present || configStore.value.HasPairingBinding() || configStore.value.Agent == nil || credentialStore.present {
		t.Fatalf("forget exit=%d config=%+v credential=%+v out=%q err=%q", exit, configStore, credentialStore, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "远端设备可能仍有效") {
		t.Fatalf("forget warning missing: %q", out.String())
	}
}

func TestForgetLocalRepairsPendingJournalWithoutNetwork(t *testing.T) {
	t.Parallel()
	configStore, credentialStore := pairedStores(config.DefaultServerURL, "token")
	modelPreset := config.DefaultAgentConfig("openrouter")
	configStore.value.Agent = &modelPreset
	configStore.journalPresent = true
	configStore.journal = config.PairingJournal{SchemaVersion: 1, ServerURL: config.DefaultServerURL, DeviceID: testDeviceID, DisplayName: "Laptop"}
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{confirmed: true})
	app.NewClient = func(string, string, time.Duration) APIClient { panic("forget-local must not create a network client") }
	if exit := app.Run(t.Context(), []string{"device", "forget-local"}); exit != ExitOK {
		t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if !configStore.present || configStore.value.HasPairingBinding() || configStore.value.Agent == nil || credentialStore.present || configStore.journalPresent {
		t.Fatalf("pending local state remains or client settings were lost: config=%+v credential=%+v", configStore, credentialStore)
	}
	if !strings.Contains(out.String(), "远端设备可能仍有效") {
		t.Fatalf("remote warning missing: %q", out.String())
	}
}

func TestKnowledgeImportRefreshesStaleIdentityReviewWithoutReusingDecision(t *testing.T) {
	t.Parallel()
	receiptOne := strings.Repeat("a", 64)
	receiptTwo := strings.Repeat("b", 64)
	locator := strings.Repeat("c", 64)
	operationIDs := []string{
		"40000000-0000-4000-8000-000000000001",
		"40000000-0000-4000-8000-000000000002",
		"40000000-0000-4000-8000-000000000003",
	}
	var imports []api.ImportRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/knowledge/revisions/head" {
			writeJSONTest(w, http.StatusNotFound, api.ErrorResponse{Error: api.ErrorBody{Code: "not_found", Message: "missing", RequestID: "head-request"}})
			return
		}
		if r.URL.Path != "/v1/knowledge/imports" {
			http.NotFound(w, r)
			return
		}
		var request api.ImportRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		imports = append(imports, request)
		switch len(imports) {
		case 1:
			writeReview(t, w, request.OperationID, receiptOne, locator)
		case 2:
			writeJSONTest(w, http.StatusConflict, api.ErrorResponse{Error: api.ErrorBody{Code: "stale_identity_review", Message: "stale", RequestID: "stale-request"}})
		case 3:
			writeReview(t, w, request.OperationID, receiptTwo, locator)
		case 4:
			writeJSONTest(w, http.StatusCreated, api.ImportResult{Revision: testRevision(), Unchanged: false})
		default:
			t.Fatalf("unexpected import request %d", len(imports))
		}
	}))
	defer server.Close()
	root := t.TempDir()
	markdownPath := filepath.Join(root, "note.md")
	if err := os.WriteFile(markdownPath, []byte("# Note\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configStore, credentialStore := pairedStores(server.URL, "token")
	terminal := &fakeTerminal{lines: []string{"preserve", testDocID, "same document", "new", "new identity after restart"}}
	app, out, errOut := newTestApp(configStore, credentialStore, terminal)
	app.NewUUID = uuidSequence(t, operationIDs...)
	if exit := app.Run(t.Context(), []string{"knowledge", "import", markdownPath}); exit != ExitOK {
		t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if len(imports) != 4 {
		t.Fatalf("imports=%d", len(imports))
	}
	if imports[0].ExpectedParentRevisionID != nil || imports[0].Source != "go-cli-m1" || len(imports[0].Documents) != 1 || imports[0].Documents[0].Path != "note.md" || strings.Contains(imports[0].Source, root) {
		t.Fatalf("initial request = %+v", imports[0])
	}
	if imports[1].IdentityReviewReceipt != receiptOne || len(imports[1].DocumentResolutions) != 1 || imports[1].DocumentResolutions[0].Action != "preserve" {
		t.Fatalf("first resolution = %+v", imports[1])
	}
	if imports[2].OperationID != imports[0].OperationID || imports[2].IdentityReviewReceipt != "" || len(imports[2].DocumentResolutions) != 0 {
		t.Fatalf("refresh request = %+v", imports[2])
	}
	if imports[3].IdentityReviewReceipt != receiptTwo || imports[3].DocumentResolutions[0].Action != "new" || imports[3].OperationID == imports[1].OperationID {
		t.Fatalf("fresh resolution = %+v", imports[3])
	}
	combined := out.String() + errOut.String()
	if strings.Contains(combined, receiptOne) || strings.Contains(combined, receiptTwo) || !strings.Contains(combined, "previous decisions were discarded") {
		t.Fatalf("receipt leaked or stale warning missing: %q", combined)
	}
}

func TestKnowledgeImportCompletesDocumentThenNodeReview(t *testing.T) {
	t.Parallel()
	documentLocator := strings.Repeat("1", 64)
	nodeLocator := strings.Repeat("2", 64)
	nodeRevisionID := "90000000-0000-4000-8000-000000000001"
	var imports []api.ImportRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/knowledge/revisions/head" {
			writeJSONTest(w, http.StatusNotFound, api.ErrorResponse{Error: api.ErrorBody{Code: "not_found", Message: "missing", RequestID: "head-request"}})
			return
		}
		var request api.ImportRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		imports = append(imports, request)
		switch len(imports) {
		case 1:
			writeJSONTest(w, http.StatusConflict, api.ErrorResponse{Error: api.ErrorBody{Code: "identity_review_required", Message: "document", RequestID: "review-document"}, IdentityReview: &api.IdentityReview{
				BasisHash: strings.Repeat("a", 64), OperationID: request.OperationID, Receipt: strings.Repeat("b", 64), Nodes: []api.NodeIdentityReview{},
				Documents: []api.DocumentIdentityReview{{Path: "note.md", Locator: documentLocator, ReasonCode: "ambiguous", Candidates: []api.IdentityCandidate{{StableID: testDocID, RevisionID: testDocRevID, ReasonCode: "same_path"}}}},
			}})
		case 2:
			if len(request.DocumentResolutions) != 1 || len(request.NodeResolutions) != 0 {
				t.Fatalf("document stage request = %+v", request)
			}
			writeJSONTest(w, http.StatusConflict, api.ErrorResponse{Error: api.ErrorBody{Code: "identity_review_required", Message: "node", RequestID: "review-node"}, IdentityReview: &api.IdentityReview{
				BasisHash: strings.Repeat("c", 64), OperationID: request.OperationID, Receipt: strings.Repeat("d", 64), Documents: []api.DocumentIdentityReview{},
				Nodes: []api.NodeIdentityReview{{Path: "note.md", Locator: nodeLocator, Preorder: 0, ReasonCode: "ambiguous", Candidates: []api.IdentityCandidate{{StableID: testDocID, RevisionID: nodeRevisionID, ReasonCode: "semantic"}}}},
			}})
		case 3:
			if len(request.DocumentResolutions) != 1 || len(request.NodeResolutions) != 1 || request.DocumentResolutions[0].Locator != documentLocator || request.NodeResolutions[0].Locator != nodeLocator {
				t.Fatalf("combined stage request = %+v", request)
			}
			writeJSONTest(w, http.StatusCreated, api.ImportResult{Revision: testRevision()})
		default:
			t.Fatalf("unexpected import request %d", len(imports))
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "note.md")
	if err := os.WriteFile(path, []byte("# Note\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configStore, credentialStore := pairedStores(server.URL, "token")
	terminal := &fakeTerminal{lines: []string{"preserve", testDocID, "same document", "rewrite", nodeRevisionID, "same node"}}
	app, out, errOut := newTestApp(configStore, credentialStore, terminal)
	app.NewUUID = uuidSequence(t,
		"91000000-0000-4000-8000-000000000001",
		"91000000-0000-4000-8000-000000000002",
		"91000000-0000-4000-8000-000000000003",
	)
	if exit := app.Run(t.Context(), []string{"knowledge", "import", path}); exit != ExitOK {
		t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	if len(imports) != 3 {
		t.Fatalf("imports=%d", len(imports))
	}
}

func TestKnowledgeImportRejectsOversizeBeforeAnyHTTP(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	content := strings.Repeat(`"`, 2<<20)
	for index := 0; index < 4; index++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("%d.md", index)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	app, _, errOut := newTestApp(&memoryConfigStore{}, &memoryCredentialStore{}, &fakeTerminal{})
	app.NewClient = func(string, string, time.Duration) APIClient {
		panic("oversized import must not create an HTTP client")
	}
	if exit := app.Run(t.Context(), []string{"knowledge", "import", root}); exit != ExitInput || !strings.Contains(errOut.String(), "payload_too_large") {
		t.Fatalf("exit=%d err=%q", exit, errOut.String())
	}
}

func TestPairEscapesServiceControlCharacters(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSONTest(w, http.StatusCreated, api.IssuedCredential{Device: testDevice("Laptop\x1b[2J\nInjected"), Token: "token"})
	}))
	defer server.Close()
	app, out, errOut := newTestApp(&memoryConfigStore{}, &memoryCredentialStore{}, &fakeTerminal{secret: "pair-code"})
	if exit := app.Run(t.Context(), []string{"pair", "--server", server.URL, "--name", "Laptop"}); exit != ExitOK {
		t.Fatalf("exit=%d err=%q", exit, errOut.String())
	}
	if strings.ContainsRune(out.String(), '\x1b') || strings.Contains(out.String(), "\nInjected\n") || !strings.Contains(out.String(), `\u001b[2J\nInjected`) {
		t.Fatalf("unsafe output %q", out.String())
	}
}

func TestKnowledgeImportStopsOnRevisionConflict(t *testing.T) {
	t.Parallel()
	current := "50000000-0000-4000-8000-000000000001"
	var imports atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/knowledge/revisions/head":
			writeJSONTest(w, http.StatusOK, api.HeadResponse{Revision: testRevision()})
		case "/v1/knowledge/imports":
			imports.Add(1)
			writeJSONTest(w, http.StatusConflict, api.ErrorResponse{Error: api.ErrorBody{Code: "revision_conflict", Message: "conflict", RequestID: "request-conflict"}, CurrentRevisionID: &current})
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "note.md")
	if err := os.WriteFile(path, []byte("# Note\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configStore, credentialStore := pairedStores(server.URL, "token")
	app, _, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	app.NewUUID = uuidSequence(t, "60000000-0000-4000-8000-000000000001")
	if exit := app.Run(t.Context(), []string{"knowledge", "import", path}); exit != ExitConflict || imports.Load() != 1 {
		t.Fatalf("exit=%d imports=%d err=%q", exit, imports.Load(), errOut.String())
	}
	if !strings.Contains(errOut.String(), current) {
		t.Fatalf("current head missing: %q", errOut.String())
	}
}

func newTestApp(configStore ConfigStore, credentialStore CredentialStore, term Terminal) (*App, *bytes.Buffer, *bytes.Buffer) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	return &App{
		Config: configStore, Credentials: credentialStore, Terminal: term, Out: out, Err: errOut,
		Getenv: func(string) string { return "" }, NewUUID: func() (string, error) { return "70000000-0000-4000-8000-000000000001", nil },
		NewClient: func(serverURL, token string, timeout time.Duration) APIClient {
			return api.NewClient(serverURL, token, timeout, nil)
		},
	}, out, errOut
}

func pairedStores(serverURL, token string) (*memoryConfigStore, *memoryCredentialStore) {
	return &memoryConfigStore{present: true, value: config.Config{ServerURL: serverURL, DeviceID: testDeviceID, DisplayName: "Laptop", Timeout: "2s", Color: "never"}},
		&memoryCredentialStore{present: true, record: credentials.Record{ServerURL: serverURL, DeviceID: testDeviceID, Token: token}}
}

func testDevice(name string) api.Device {
	return api.Device{ID: testDeviceID, DisplayName: name, Scopes: []string{"model:probe", "devices:read", "devices:manage"}, CreatedAt: time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)}
}

func testRevision() api.KnowledgeRevision {
	return api.KnowledgeRevision{
		RevisionID: "80000000-0000-4000-8000-000000000001", RevisionNo: 1,
		ManifestHash: strings.Repeat("d", 64), Source: "go-cli-m1", CreatedByDeviceID: testDeviceID,
		CreatedAt: time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC), CanonicalizerVersion: "edu-markdown-v1",
		ParserVersion: "goldmark-v1.8.5-commonmark-0.31.2-gfm", IndexerVersion: "knowledge-indexer-v1", IdentityPolicyVersion: "identity-policy-v1",
	}
}

func writeReview(t *testing.T, w http.ResponseWriter, operationID, receipt, locator string) {
	t.Helper()
	review := api.IdentityReview{
		BasisHash: strings.Repeat("e", 64), OperationID: operationID, Receipt: receipt,
		Documents: []api.DocumentIdentityReview{{
			Path: "note.md", Locator: locator, ReasonCode: "document_match_ambiguous",
			Candidates: []api.IdentityCandidate{{StableID: testDocID, RevisionID: testDocRevID, ReasonCode: "same_path", Score: 500000, Evidence: map[string]any{"path": "note.md", "unsafe": map[string]any{"raw": "hidden"}}}},
		}}, Nodes: []api.NodeIdentityReview{},
	}
	writeJSONTest(w, http.StatusConflict, api.ErrorResponse{Error: api.ErrorBody{Code: "identity_review_required", Message: "review", RequestID: "review-request"}, IdentityReview: &review})
}

func writeJSONTest(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func uuidSequence(t *testing.T, values ...string) func() (string, error) {
	t.Helper()
	index := 0
	return func() (string, error) {
		if index >= len(values) {
			return "", fmt.Errorf("UUID sequence exhausted")
		}
		value := values[index]
		index++
		return value, nil
	}
}

func TestClearCommandDoesNotUseExternalProcess(t *testing.T) {
	t.Parallel()
	app, _, errOut := newTestApp(&memoryConfigStore{}, &memoryCredentialStore{}, &fakeTerminal{clearErr: terminal.ErrNotTerminal})
	if exit := app.Run(context.Background(), []string{"clear"}); exit != ExitInput || !strings.Contains(errOut.String(), "not_a_terminal") {
		t.Fatalf("exit=%d err=%q", exit, errOut.String())
	}
}
