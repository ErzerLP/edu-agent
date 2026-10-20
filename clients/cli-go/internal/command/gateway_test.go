package command

import (
	"fmt"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGatewayPrivacyBeforeAnyRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); t.Error("request before consent") }))
	defer server.Close()
	configs, credentials := pairedStores(server.URL, "token")
	app, _, errOut := newTestApp(configs, credentials, &fakeTerminal{})
	if code := app.Run(t.Context(), []string{"model", "test"}); code == ExitOK {
		t.Fatal("allowed without consent")
	}
	if calls.Load() != 0 || !strings.Contains(errOut.String(), "问题、相关对话和工具返回的本地内容") {
		t.Fatalf("calls=%d error=%s", calls.Load(), errOut.String())
	}
}

func TestGatewayUnpairedDoesNotInvoke(t *testing.T) {
	app, _, errOut := newTestApp(&memoryConfigStore{}, &memoryCredentialStore{}, &fakeTerminal{})
	if code := app.Run(t.Context(), []string{"model", "test"}); code != ExitAuth || !strings.Contains(errOut.String(), "配对") {
		t.Fatalf("exit=%d %s", code, errOut.String())
	}
}

func TestGatewaySelectionRememberedAndInvalidated(t *testing.T) {
	var revision, destination atomic.Int32
	var unavailable, disabled atomic.Bool
	var calls atomic.Int32
	revision.Store(1)
	destination.Store(1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/model-gateway/config" {
			if unavailable.Load() {
				http.Error(w, "unavailable", 503)
				return
			}
			target := "mentor"
			if disabled.Load() {
				target = "teaching"
			}
			fmt.Fprintf(w, `{"protocol_version":1,"invoke_allowed":true,"available":true,"privacy_generation":7,"models":[{"target":"%s","display_name":"AI 助手模型","model_name":"demo","configuration_revision":"c%d","destination_revision":"d%d","context_tokens":32768,"output_tokens":2048}],"limits":{"idle_timeout_seconds":60,"request_timeout_seconds":900}}`, target, revision.Load(), destination.Load())
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"正常"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	configs, credentials := pairedStores(server.URL, "token")
	terminal := &fakeTerminal{confirmed: true}
	app, _, errOut := newTestApp(configs, credentials, terminal)
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }
	if code := app.Run(t.Context(), []string{"model", "select", "mentor"}); code != ExitOK {
		t.Fatal(errOut.String())
	}
	app2, _, errs := newTestApp(configs, credentials, &fakeTerminal{})
	if code := app2.Run(t.Context(), []string{"model", "test"}); code != ExitOK || calls.Load() != 1 {
		t.Fatalf("remembered: %d %s", code, errs.String())
	}
	revision.Store(2)
	if code := app2.Run(t.Context(), []string{"model", "test"}); code != ExitOK || calls.Load() != 2 {
		t.Fatalf("configuration-only change required consent: %d %s", code, errs.String())
	}
	destination.Store(2)
	if code := app2.Run(t.Context(), []string{"model", "test"}); code == ExitOK || calls.Load() != 2 || !strings.Contains(errs.String(), "所选模型服务已更改") {
		t.Fatalf("changed: %d calls=%d %s", code, calls.Load(), errs.String())
	}
	if code := app.Run(t.Context(), []string{"model", "select", "teaching"}); code == ExitOK || calls.Load() != 2 {
		t.Fatal("selected absent teaching")
	}
	terminal.confirmed = false
	if code := app.Run(t.Context(), []string{"model", "select", "mentor"}); code == ExitOK || configs.value.Gateway.DestinationRevision != "d1" {
		t.Fatal("declined destination was saved")
	}
	terminal.confirmed = true
	if code := app.Run(t.Context(), []string{"model", "select", "mentor"}); code != ExitOK || configs.value.Gateway.DestinationRevision != "d2" {
		t.Fatal("confirmed destination not remembered")
	}
	disabled.Store(true)
	if code := app2.Run(t.Context(), []string{"model", "test"}); code == ExitOK || calls.Load() != 2 || configs.value.Gateway.SelectedTarget != "mentor" {
		t.Fatal("disabled selection fell back")
	}
	unavailable.Store(true)
	if code := app2.Run(t.Context(), []string{"model", "test"}); code == ExitOK || calls.Load() != 2 {
		t.Fatal("unavailable config allowed model call")
	}

}

func TestGatewayChangingServerRequiresFreshNotice(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	configs, creds := pairedStores("https://old.example", "token")
	configs.value.Gateway = &config.GatewayState{ServerURL: configs.value.ServerURL, DeviceID: configs.value.DeviceID, SelectedTarget: "teaching", DestinationRevision: "old", NoticeVersion: 1}
	configs.value = configs.value.WithoutPairing()
	configs.value.ServerURL = server.URL
	configs.value.DeviceID = creds.record.DeviceID
	configs.value.DisplayName = "new device"
	creds.record.ServerURL = server.URL
	app, _, errors := newTestApp(configs, creds, &fakeTerminal{})
	if code := app.Run(t.Context(), []string{"model", "test"}); code == ExitOK || calls.Load() != 0 || !strings.Contains(errors.String(), "问题、相关对话") {
		t.Fatalf("server switch skipped notice: %d %s", code, errors.String())
	}
}

func TestRemovedModelCommandsCannotConfigureDirectConnection(t *testing.T) {
	app, _, _ := newTestApp(&memoryConfigStore{}, &memoryCredentialStore{}, &fakeTerminal{})
	for _, args := range [][]string{{"preset", "openai"}, {"key", "delete", "--confirmed"}, {"set", "--provider", "custom"}, {"set", "--base-url", "https://model.example"}, {"set", "--model", "arbitrary"}, {"set", "--max-tokens", "4096"}, {"set", "--context-window", "32768"}, {"set", "--timeout", "1m"}} {
		if code := app.Run(t.Context(), append([]string{"model"}, args...)); code != ExitInput {
			t.Fatalf("accepted direct setting: %v code=%d", args, code)
		}
	}
}
