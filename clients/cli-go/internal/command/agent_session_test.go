package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentcontroller"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentui"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/keybackend"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

type commandSessionSecrets struct {
	mu    sync.Mutex
	value []byte
}

func (*commandSessionSecrets) Available(keybackend.Locator) error { return nil }
func (s *commandSessionSecrets) Load(keybackend.Locator, int) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.value) == 0 {
		return nil, keybackend.ErrNotFound
	}
	return append([]byte(nil), s.value...), nil
}
func (s *commandSessionSecrets) Store(_ keybackend.Locator, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = append([]byte(nil), value...)
	return nil
}
func (s *commandSessionSecrets) Delete(keybackend.Locator) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = nil
	return nil
}

type sharedPickerAgentUI struct {
	fakeAgentUI
	pickCalls int
	listed    []agentcontroller.SessionListItem
}

func (u *sharedPickerAgentUI) Pick(ctx context.Context, source agentui.SessionPickerSource, all bool) (agentui.PickerChoice, error) {
	u.pickCalls++
	items, err := source.ListSessions(ctx, agentcontroller.SessionListRequest{All: all})
	if err != nil {
		return agentui.PickerChoice{}, err
	}
	u.listed = items
	if len(items) == 0 {
		return agentui.PickerChoice{Cancelled: true}, nil
	}
	return agentui.PickerChoice{SessionID: items[0].Summary.SessionID}, nil
}

func TestAgentResumeWithoutSelectorUsesSharedPicker(t *testing.T) {
	preset := config.DefaultAgentConfig("ollama")
	configStore, credentialStore := pairedStores(config.DefaultServerURL, "server-token")
	configStore.value.Agent = &preset
	root := t.TempDir()
	secrets := &commandSessionSecrets{}
	model := &requestCapturingAgentModel{response: modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "已保存的回答"}}}
	first := &singleSendAgentUI{}
	app, _, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	app.ModelSecrets = &memoryModelSecretStore{}
	app.AgentUI = first
	app.NewModel = func(config.AgentConfig, string) (agentloop.Model, error) { return model, nil }
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }
	app.Getenv = func(string) string { return "xterm" }
	app.AgentSessionRoot = func() (string, error) { return root, nil }
	app.AgentSessionSecrets = secrets
	if exit := app.Run(t.Context(), []string{"agent"}); exit != ExitOK || first.err != nil {
		t.Fatalf("new exit=%d send=%v err=%q", exit, first.err, errOut.String())
	}

	picker := &sharedPickerAgentUI{}
	app.AgentUI = picker
	errOut.Reset()
	if exit := app.Run(t.Context(), []string{"agent", "resume"}); exit != ExitOK {
		t.Fatalf("resume exit=%d err=%q", exit, errOut.String())
	}
	if picker.pickCalls != 1 || picker.calls != 1 || len(picker.listed) != 1 || picker.conversation == nil {
		t.Fatalf("pickCalls=%d runCalls=%d listed=%+v conversation=%T", picker.pickCalls, picker.calls, picker.listed, picker.conversation)
	}
}

func TestAgentResumeArgumentMatrix(t *testing.T) {
	tests := []struct {
		args      []string
		target    string
		last, all bool
		wantErr   bool
	}{
		{args: []string{"title", "--all"}, target: "title", all: true},
		{args: []string{"--last"}, last: true},
		{args: []string{"--all"}, all: true},
		{args: []string{"title", "--last"}, wantErr: true},
		{args: []string{"--last", "--all"}, wantErr: true},
		{args: []string{"one", "two"}, wantErr: true},
		{args: []string{"--workspace", "."}, wantErr: true},
		{args: []string{"--unknown"}, wantErr: true},
	}
	for _, test := range tests {
		target, last, all, _, err := parseAgentResumeArgs(test.args)
		if (err != nil) != test.wantErr || target != test.target || last != test.last || all != test.all {
			t.Fatalf("args=%v target=%q last=%t all=%t err=%v", test.args, target, last, all, err)
		}
	}
}

func TestAgentSessionSelectionUsesUUIDThenUniqueFoldedTitleAndWorkspaceScope(t *testing.T) {
	current := agentsession.Summary{SessionID: "10000000-0000-4000-8000-000000000001", Title: "Café", WorkspaceID: "current", UpdatedAt: time.Now()}
	other := agentsession.Summary{SessionID: "10000000-0000-4000-8000-000000000002", Title: "CAFE\u0301", WorkspaceID: "other", UpdatedAt: time.Now().Add(-time.Minute)}

	selected, err := selectSummary([]agentsession.Summary{current, other}, "cafe\u0301", false, "current")
	if err != nil || selected.SessionID != current.SessionID {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
	if _, err := selectSummary([]agentsession.Summary{current, other}, "CAFÉ", true, ""); err == nil || !containsCommandCode(err, "session_name_ambiguous") {
		t.Fatalf("ambiguous title err=%v", err)
	}
	selected, err = selectSummary([]agentsession.Summary{current, other}, other.SessionID, true, "")
	if err != nil || selected.SessionID != other.SessionID {
		t.Fatalf("uuid selected=%+v err=%v", selected, err)
	}
	app := &App{}
	latest, err := app.selectAgentSession([]agentsession.Summary{other, current}, "", true, false, "current")
	if err != nil || latest.SessionID != current.SessionID {
		t.Fatalf("--last selection=%+v err=%v", latest, err)
	}
}

func TestAgentLastSelectionSkipsLockedUnavailableAndCorrupt(t *testing.T) {
	base := time.Date(2025, time.January, 2, 15, 4, 5, 0, time.UTC)
	locked := agentsession.Summary{
		SessionID: "10000000-0000-4000-8000-000000000011", WorkspaceID: "current",
		UpdatedAt: base.Add(3 * time.Minute), Locked: true,
	}
	unavailable := agentsession.Summary{
		SessionID: "10000000-0000-4000-8000-000000000012", WorkspaceID: "current",
		UpdatedAt: base.Add(2 * time.Minute), Unavailable: true,
	}
	corrupt := agentsession.Summary{
		SessionID: "10000000-0000-4000-8000-000000000013", WorkspaceID: "current",
		UpdatedAt: base.Add(time.Minute), Corrupt: true,
	}
	recoverable := agentsession.Summary{
		SessionID: "10000000-0000-4000-8000-000000000014", WorkspaceID: "current",
		UpdatedAt: base,
	}
	otherWorkspace := agentsession.Summary{
		SessionID: "10000000-0000-4000-8000-000000000015", WorkspaceID: "other",
		UpdatedAt: base.Add(4 * time.Minute),
	}

	app := &App{}
	selected, err := app.selectAgentSession([]agentsession.Summary{
		recoverable, corrupt, otherWorkspace, unavailable, locked,
	}, "", true, false, "current")
	if err != nil || selected.SessionID != recoverable.SessionID {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
}

func TestAgentLastSelectionReturnsNotFoundWhenAllInScopeAreUnavailable(t *testing.T) {
	base := time.Date(2025, time.January, 2, 15, 4, 5, 0, time.UTC)
	app := &App{}
	_, err := app.selectAgentSession([]agentsession.Summary{
		{SessionID: "10000000-0000-4000-8000-000000000021", WorkspaceID: "current", UpdatedAt: base.Add(3 * time.Minute), Locked: true},
		{SessionID: "10000000-0000-4000-8000-000000000022", WorkspaceID: "current", UpdatedAt: base.Add(2 * time.Minute), Unavailable: true},
		{SessionID: "10000000-0000-4000-8000-000000000023", WorkspaceID: "current", UpdatedAt: base.Add(time.Minute), Corrupt: true},
		{SessionID: "10000000-0000-4000-8000-000000000024", WorkspaceID: "other", UpdatedAt: base.Add(4 * time.Minute)},
	}, "", true, false, "current")
	if !containsCommandCode(err, "session_not_found") {
		t.Fatalf("err=%v", err)
	}
	commandErr, ok := err.(*Error)
	if !ok || commandErr.Detail == "" || commandErr.Next == "" {
		t.Fatalf("not-found error lacks recovery prompt: %v", err)
	}
}

func TestAgentExplicitTargetDoesNotSkipLockedOrCorruptSummary(t *testing.T) {
	locked := agentsession.Summary{
		SessionID: "10000000-0000-4000-8000-000000000031", Title: "锁定会话", WorkspaceID: "current", Locked: true,
	}
	corrupt := agentsession.Summary{
		SessionID: "10000000-0000-4000-8000-000000000032", Title: "损坏会话", WorkspaceID: "current", Corrupt: true,
	}
	app := &App{}

	selected, err := app.selectAgentSession([]agentsession.Summary{locked, corrupt}, locked.SessionID, false, false, "current")
	if err != nil || selected.SessionID != locked.SessionID || !selected.Locked {
		t.Fatalf("locked target selected=%+v err=%v", selected, err)
	}
	selected, err = app.selectAgentSession([]agentsession.Summary{locked, corrupt}, corrupt.Title, false, false, "current")
	if err != nil || selected.SessionID != corrupt.SessionID || !selected.Corrupt {
		t.Fatalf("corrupt target selected=%+v err=%v", selected, err)
	}
}

func TestAgentSessionHistoryOffSkipsStoreWithoutChangingConfig(t *testing.T) {
	preset := config.DefaultAgentConfig("ollama")
	preset.SessionHistory = "off"
	configStore, credentialStore := pairedStores(config.DefaultServerURL, "server-token")
	configStore.value.Agent = &preset
	model := &requestCapturingAgentModel{response: modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "回答"}}}
	runner := &singleSendAgentUI{}
	app, _, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	app.ModelSecrets = &memoryModelSecretStore{}
	app.AgentUI = runner
	app.NewModel = func(config.AgentConfig, string) (agentloop.Model, error) { return model, nil }
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }
	app.Getenv = func(string) string { return "xterm" }
	app.AgentSessionRoot = func() (string, error) {
		t.Fatal("session-history=off opened the session store")
		return "", nil
	}

	beforeSaves := configStore.saveCalls
	if exit := app.Run(t.Context(), []string{"agent"}); exit != ExitOK || runner.err != nil {
		t.Fatalf("exit=%d send=%v err=%q", exit, runner.err, errOut.String())
	}
	if configStore.value.Agent.SessionHistory != "off" || configStore.saveCalls != beforeSaves || len(model.requests) != 1 {
		t.Fatalf("history=%q saves=%d requests=%d", configStore.value.Agent.SessionHistory, configStore.saveCalls-beforeSaves, len(model.requests))
	}
}

func TestModelAndClientConfigSetSessionHistory(t *testing.T) {
	preset := config.DefaultAgentConfig("ollama")
	configStore, credentialStore := pairedStores(config.DefaultServerURL, "server-token")
	configStore.value.Agent = &preset
	app, _, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	if exit := app.Run(t.Context(), []string{"model", "set", "--session-history", "off"}); exit != ExitOK || configStore.value.Agent.SessionHistory != "off" {
		t.Fatalf("model set exit=%d history=%q err=%q", exit, configStore.value.Agent.SessionHistory, errOut.String())
	}
	errOut.Reset()
	if exit := app.Run(t.Context(), []string{"model", "preset", "ollama"}); exit != ExitOK || configStore.value.Agent.SessionHistory != "off" {
		t.Fatalf("preset exit=%d history=%q err=%q", exit, configStore.value.Agent.SessionHistory, errOut.String())
	}
	errOut.Reset()
	if exit := app.Run(t.Context(), []string{"config", "set", "--session-history", "auto"}); exit != ExitOK || configStore.value.Agent.SessionHistory != "auto" {
		t.Fatalf("config set exit=%d history=%q err=%q", exit, configStore.value.Agent.SessionHistory, errOut.String())
	}
}

func TestAgentNoSaveSkipsSessionStoreAndTitleRequest(t *testing.T) {
	preset := config.DefaultAgentConfig("ollama")
	configStore, credentialStore := pairedStores(config.DefaultServerURL, "server-token")
	configStore.value.Agent = &preset
	model := &requestCapturingAgentModel{response: modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "回答"}}}
	runner := &singleSendAgentUI{}
	app, _, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	app.ModelSecrets = &memoryModelSecretStore{}
	app.AgentUI = runner
	app.NewModel = func(config.AgentConfig, string) (agentloop.Model, error) { return model, nil }
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }
	app.Getenv = func(string) string { return "xterm" }
	app.AgentSessionRoot = func() (string, error) {
		t.Fatal("--no-save opened the session store")
		return "", nil
	}

	beforeSaves := configStore.saveCalls
	if exit := app.Run(t.Context(), []string{"agent", "--no-save"}); exit != ExitOK || runner.err != nil {
		t.Fatalf("exit=%d send=%v err=%q", exit, runner.err, errOut.String())
	}
	if len(model.requests) != 1 || configStore.saveCalls != beforeSaves || configStore.value.Agent.SessionHistory != "auto" {
		t.Fatalf("requests=%d saves=%d history=%q", len(model.requests), configStore.saveCalls-beforeSaves, configStore.value.Agent.SessionHistory)
	}
}

func TestAgentSessionHelpAndConfirmationParsingAreDependencyFree(t *testing.T) {
	app, out, errOut := newTestApp(&memoryConfigStore{}, &memoryCredentialStore{}, &fakeTerminal{})
	if exit := app.Run(t.Context(), []string{"agent", "sessions", "--help"}); exit != ExitOK || out.Len() == 0 || errOut.Len() != 0 {
		t.Fatalf("help exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	out.Reset()
	if exit := app.Run(t.Context(), []string{"agent", "sessions", "clear"}); exit != ExitInput || !containsText(errOut.String(), "confirmation_required") {
		t.Fatalf("clear exit=%d err=%q", exit, errOut.String())
	}
}

func TestAgentSessionsDeleteAndClearEncryptedRecords(t *testing.T) {
	root := t.TempDir()
	secrets := &commandSessionSecrets{}
	profile, err := agentsession.ProfileFingerprint(config.DefaultServerURL)
	if err != nil {
		t.Fatal(err)
	}
	profileRoot := filepath.Join(root, profile)
	create := func(title string) string {
		store, openErr := agentsession.Open(t.Context(), agentsession.Options{Root: profileRoot, ProfileFingerprint: profile, Secrets: secrets})
		if openErr != nil {
			t.Fatal(openErr)
		}
		transcript, encodeErr := agentsession.EncodeTranscript(agentsession.TranscriptV1{SchemaVersion: 1, Entries: []agentsession.TranscriptEntryV1{}}, agentsession.DefaultLimits())
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		handle, record, createErr := store.Create(t.Context(), agentsession.CreateInput{Title: title, Checkpoint: []byte(`{}`), Transcript: transcript})
		if createErr != nil {
			t.Fatal(createErr)
		}
		_ = handle.Close()
		_ = store.Close()
		return record.SessionID
	}
	firstID := create("第一节")

	configStore, credentialStore := pairedStores(config.DefaultServerURL, "server-token")
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	app.AgentSessionRoot = func() (string, error) { return root, nil }
	app.AgentSessionSecrets = secrets
	if exit := app.Run(t.Context(), []string{"agent", "sessions", "delete", firstID, "--confirmed"}); exit != ExitOK || !containsText(out.String(), firstID) {
		t.Fatalf("delete exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}

	create("第二节")
	out.Reset()
	errOut.Reset()
	if exit := app.Run(t.Context(), []string{"agent", "sessions", "clear", "--confirmed"}); exit != ExitOK {
		t.Fatalf("clear exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	store, err := agentsession.Open(t.Context(), agentsession.Options{Root: profileRoot, ProfileFingerprint: profile, Secrets: secrets})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	listed, err := store.List(t.Context())
	if err != nil || len(listed) != 0 {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
}

func TestAgentSessionsDeleteTrustedCorruptUUIDAndExplicitStorageLocator(t *testing.T) {
	root := t.TempDir()
	secrets := &commandSessionSecrets{}
	profile, err := agentsession.ProfileFingerprint(config.DefaultServerURL)
	if err != nil {
		t.Fatal(err)
	}
	profileRoot := filepath.Join(root, profile)
	create := func(title string) agentsession.SessionRecord {
		store, openErr := agentsession.Open(t.Context(), agentsession.Options{Root: profileRoot, ProfileFingerprint: profile, Secrets: secrets})
		if openErr != nil {
			t.Fatal(openErr)
		}
		handle, record, createErr := store.Create(t.Context(), agentsession.CreateInput{Title: title, Checkpoint: []byte(`{}`)})
		if createErr != nil {
			t.Fatal(createErr)
		}
		_ = handle.Close()
		_ = store.Close()
		return record
	}
	tamperEnvelope := func(record agentsession.SessionRecord) {
		path := filepath.Join(profileRoot, "key-"+record.StorageID+".enc")
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		raw[len(raw)-1] ^= 0x40
		if writeErr := os.WriteFile(path, raw, 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	configStore, credentialStore := pairedStores(config.DefaultServerURL, "server-token")
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	app.AgentSessionRoot = func() (string, error) { return root, nil }
	app.AgentSessionSecrets = secrets

	trusted := create("trusted corrupt")
	tamperEnvelope(trusted)
	if exit := app.Run(t.Context(), []string{"agent", "sessions", "delete", trusted.SessionID, "--confirmed"}); exit != ExitOK || !containsText(out.String(), trusted.SessionID) {
		t.Fatalf("trusted corrupt delete exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}

	locator := create("locator only")
	if err := os.Remove(filepath.Join(profileRoot, "profile.index.enc")); err != nil {
		t.Fatal(err)
	}
	tamperEnvelope(locator)
	out.Reset()
	errOut.Reset()
	selector := "storage:" + locator.StorageID
	if exit := app.Run(t.Context(), []string{"agent", "sessions", "delete", selector, "--confirmed"}); exit != ExitOK || !containsText(out.String(), selector) {
		t.Fatalf("locator delete exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if exit := app.Run(t.Context(), []string{"agent", "sessions", "delete", "storage:" + strings.ToUpper(locator.StorageID), "--confirmed"}); exit != ExitInput {
		t.Fatalf("uppercase locator exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
}

func TestCanonicalStorageSelectorIsStrict(t *testing.T) {
	valid := "storage:" + strings.Repeat("a", 32)
	if storageID, ok := canonicalStorageSelector(valid); !ok || storageID != strings.Repeat("a", 32) {
		t.Fatalf("valid selector storage=%q ok=%t", storageID, ok)
	}
	for _, value := range []string{"storage:" + strings.Repeat("A", 32), "storage:" + strings.Repeat("a", 31), "storage:" + strings.Repeat("g", 32), "prefix:" + strings.Repeat("a", 32)} {
		if _, ok := canonicalStorageSelector(value); ok {
			t.Fatalf("invalid selector accepted: %q", value)
		}
	}
}

func TestSessionCommandErrorMapsSentinelsWithoutRawDetails(t *testing.T) {
	rawPrefix := "schema_version=99 message_turn_ids[0]=turn-999 key-account=/private provider-body /home/secret"
	tests := []struct {
		name  string
		cause error
		code  string
		exit  int
	}{
		{name: "key unavailable", cause: agentsession.ErrKeyUnavailable, code: "session_store_unavailable", exit: ExitUnavailable},
		{name: "not found", cause: agentsession.ErrNotFound, code: "session_not_found", exit: ExitInput},
		{name: "in use", cause: agentsession.ErrInUse, code: "session_in_use", exit: ExitConflict},
		{name: "version unsupported", cause: agentsession.ErrVersionUnsupported, code: "session_version_unsupported", exit: ExitConflict},
		{name: "checkpoint version unsupported", cause: agentloop.ErrCheckpointVersionUnsupported, code: "session_version_unsupported", exit: ExitConflict},
		{name: "corrupt", cause: agentsession.ErrCorrupt, code: "session_corrupt", exit: ExitConflict},
		{name: "checkpoint corrupt", cause: agentloop.ErrCheckpointCorrupt, code: "session_corrupt", exit: ExitConflict},
		{name: "checkpoint save failed", cause: agentsession.ErrCheckpointSaveFailed, code: "session_save_failed", exit: ExitUnavailable},
		{name: "outcome unknown", cause: agentsession.ErrOutcomeUnknown, code: "session_save_failed", exit: ExitUnavailable},
		{name: "checkpoint conflict", cause: agentsession.ErrCheckpointConflict, code: "session_checkpoint_conflict", exit: ExitConflict},
		{name: "delete failed", cause: agentsession.ErrDeleteFailed, code: "session_delete_failed", exit: ExitUnavailable},
		{name: "store full", cause: agentsession.ErrStoreFull, code: "session_store_full", exit: ExitUnavailable},
		{name: "privacy invalidated", cause: agentsession.ErrPrivacyInvalidated, code: "session_privacy_invalidated", exit: ExitConflict},
		{name: "workspace confirmation", cause: agentcontroller.ErrWorkspaceConfirmationRequired, code: "workspace_confirmation_required", exit: ExitConflict},
		{name: "provider confirmation", cause: agentcontroller.ErrProviderConfirmationRequired, code: "session_provider_confirmation_required", exit: ExitConflict},
		{name: "generic", cause: fmt.Errorf("unclassified local failure"), code: "session_store_unavailable", exit: ExitUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mapped, ok := sessionCommandError(fmt.Errorf("%s: %w", rawPrefix, test.cause)).(*Error)
			if !ok {
				t.Fatalf("mapped error type=%T", sessionCommandError(test.cause))
			}
			if mapped.Code != test.code || mapped.ExitCode != test.exit || mapped.Detail == "" || mapped.Next == "" {
				t.Fatalf("mapped=%+v", mapped)
			}
			for _, forbidden := range []string{"schema_version=99", "message_turn_ids", "turn-999", "key-account", "provider-body", "/home/secret"} {
				if containsText(mapped.Error(), forbidden) {
					t.Fatalf("raw detail %q leaked in %q", forbidden, mapped.Error())
				}
			}
		})
	}
}

func TestSessionCommandErrorCoversAllSpecStableCodes(t *testing.T) {
	tests := []struct {
		name string
		code string
		exit int
	}{
		{name: "store unavailable", code: "session_store_unavailable", exit: ExitUnavailable},
		{name: "store full", code: "session_store_full", exit: ExitUnavailable},
		{name: "save failed", code: "session_save_failed", exit: ExitUnavailable},
		{name: "not found", code: "session_not_found", exit: ExitInput},
		{name: "name ambiguous", code: "session_name_ambiguous", exit: ExitConflict},
		{name: "in use", code: "session_in_use", exit: ExitConflict},
		{name: "corrupt", code: "session_corrupt", exit: ExitConflict},
		{name: "version unsupported", code: "session_version_unsupported", exit: ExitConflict},
		{name: "checkpoint conflict", code: "session_checkpoint_conflict", exit: ExitConflict},
		{name: "interrupted", code: "session_interrupted", exit: ExitConflict},
		{name: "workspace unavailable", code: "session_workspace_unavailable", exit: ExitUnavailable},
		{name: "provider confirmation", code: "session_provider_confirmation_required", exit: ExitConflict},
		{name: "privacy revalidation pending", code: "session_privacy_revalidation_pending", exit: ExitUnavailable},
		{name: "delete failed", code: "session_delete_failed", exit: ExitUnavailable},
		{name: "title failed", code: "session_title_failed", exit: ExitUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cause := fmt.Errorf("raw schema_version=99 key-account=/private provider-body tool_args=/home/secret: %w", commandError(test.code, "raw detail", "raw next", ExitInternal))
			mapped, ok := sessionCommandError(cause).(*Error)
			if !ok {
				t.Fatalf("mapped error type=%T", sessionCommandError(cause))
			}
			if mapped.Code != test.code || mapped.ExitCode != test.exit || mapped.Detail == "" || mapped.Next == "" {
				t.Fatalf("mapped=%+v", mapped)
			}
			for _, forbidden := range []string{"raw detail", "raw next", "schema_version=99", "key-account", "provider-body", "tool_args", "/home/secret"} {
				if containsText(mapped.Error(), forbidden) {
					t.Fatalf("raw detail %q leaked in %q", forbidden, mapped.Error())
				}
			}
		})
	}
}

func TestAgentCommandRestartResumeAndBoundaryMatrix(t *testing.T) {
	root := t.TempDir()
	workspaceRoot := t.TempDir()
	secrets := &commandSessionSecrets{}
	model := &commandMatrixModel{}
	firstUI := &commandMatrixUI{send: "第一轮：记住边界条件"}
	firstConfig, firstCredentials := pairedStores(config.DefaultServerURL, "server-token")
	preset := config.DefaultAgentConfig("ollama")
	firstConfig.value.Agent = &preset
	firstApp, _, firstErr := newSessionCommandApp(t, firstConfig, firstCredentials, firstUI, model, root, secrets, &fakeTerminal{})
	if exit := firstApp.Run(t.Context(), []string{"agent", "--workspace", workspaceRoot}); exit != ExitOK || firstUI.sendErr != nil {
		t.Fatalf("create exit=%d send=%v err=%q", exit, firstUI.sendErr, firstErr.String())
	}
	if firstUI.sessionID == "" {
		t.Fatal("create did not expose a persistent session id")
	}

	secondUI := &commandMatrixUI{send: "第二轮：继续回答"}
	secondConfig, secondCredentials := pairedStores(config.DefaultServerURL, "server-token")
	secondConfig.value.Agent = &preset
	secondApp, _, secondErr := newSessionCommandApp(t, secondConfig, secondCredentials, secondUI, model, root, secrets, &fakeTerminal{confirmed: true})
	if exit := secondApp.Run(t.Context(), []string{"agent", "resume", firstUI.sessionID, "--all"}); exit != ExitOK || secondUI.sendErr != nil {
		t.Fatalf("resume exit=%d send=%v err=%q", exit, secondUI.sendErr, secondErr.String())
	}
	if !secondUI.transcriptContains("第一轮：记住边界条件") {
		t.Fatalf("resumed transcript=%+v", secondUI.transcript.Entries)
	}
	if !model.requestContainsUsers("第一轮：记住边界条件", "第二轮：继续回答") {
		t.Fatalf("continued request did not contain restored history: %+v", model.snapshot())
	}

	nonTTY, out, errOut := newTestApp(&memoryConfigStore{}, &memoryCredentialStore{}, &fakeTerminal{})
	if exit := nonTTY.Run(t.Context(), []string{"agent", "resume", "--help"}); exit != ExitOK || out.Len() == 0 || errOut.Len() != 0 {
		t.Fatalf("dependency-free help exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if exit := nonTTY.Run(t.Context(), []string{"agent", "resume", "--last"}); exit != ExitInput || !containsText(errOut.String(), "not_a_terminal") {
		t.Fatalf("non-TTY resume exit=%d err=%q", exit, errOut.String())
	}
}

func TestAgentCommandProfileIsolationAndUnavailableStoreDegradation(t *testing.T) {
	t.Run("server profile isolation", func(t *testing.T) {
		root := t.TempDir()
		secrets := &commandSessionSecrets{}
		model := &commandMatrixModel{}
		preset := config.DefaultAgentConfig("ollama")
		configA, credentialsA := pairedStores(config.DefaultServerURL, "token-a")
		configA.value.Agent = &preset
		uiA := &commandMatrixUI{send: "只属于profile A"}
		appA, _, errA := newSessionCommandApp(t, configA, credentialsA, uiA, model, root, secrets, &fakeTerminal{})
		if exit := appA.Run(t.Context(), []string{"agent"}); exit != ExitOK || uiA.sendErr != nil {
			t.Fatalf("profile A exit=%d send=%v err=%q", exit, uiA.sendErr, errA.String())
		}

		serverB := "https://profile-b.example"
		configB, credentialsB := pairedStores(serverB, "token-b")
		configB.value.Agent = &preset
		uiB := &commandMatrixUI{}
		appB, _, errB := newSessionCommandApp(t, configB, credentialsB, uiB, model, root, secrets, &fakeTerminal{})
		if exit := appB.Run(t.Context(), []string{"agent", "resume", uiA.sessionID, "--all"}); exit != ExitInput || !containsText(errB.String(), "session_not_found") || uiB.calls != 0 {
			t.Fatalf("profile B exit=%d calls=%d err=%q", exit, uiB.calls, errB.String())
		}
	})

	for _, test := range []struct {
		name      string
		rootError bool
	}{
		{name: "native key unavailable"},
		{name: "store root unavailable", rootError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			preset := config.DefaultAgentConfig("ollama")
			configStore, credentialStore := pairedStores(config.DefaultServerURL, "server-token")
			configStore.value.Agent = &preset
			ui := &commandMatrixUI{send: "明文绝不能落盘 secret-boundary"}
			model := &commandMatrixModel{}
			app, _, errOut := newSessionCommandApp(t, configStore, credentialStore, ui, model, root, unavailableCommandSecrets{}, &fakeTerminal{})
			if test.rootError {
				app.AgentSessionRoot = func() (string, error) { return "", errors.New("root unavailable /private/path") }
			}
			if exit := app.Run(t.Context(), []string{"agent"}); exit != ExitOK || ui.sendErr != nil || ui.persistenceState != "unsaved" {
				t.Fatalf("new degraded exit=%d send=%v state=%q detail=%q err=%q", exit, ui.sendErr, ui.persistenceState, ui.persistenceDetail, errOut.String())
			}
			if !containsText(errOut.String(), "session_store_unavailable") || containsText(errOut.String(), "/private/path") {
				t.Fatalf("unsafe degradation output=%q", errOut.String())
			}
			assertNoPlaintextInTree(t, root, "secret-boundary")
			errOut.Reset()
			if exit := app.Run(t.Context(), []string{"agent", "resume", "--last"}); exit != ExitUnavailable || !containsText(errOut.String(), "session_store_unavailable") {
				t.Fatalf("resume fail-closed exit=%d err=%q", exit, errOut.String())
			}
			errOut.Reset()
			if exit := app.Run(t.Context(), []string{"agent", "sessions", "clear", "--confirmed"}); exit != ExitUnavailable || !containsText(errOut.String(), "session_store_unavailable") {
				t.Fatalf("management fail-closed exit=%d err=%q", exit, errOut.String())
			}
		})
	}
}

func TestAgentCommandMissingHistoricalWorkspaceRestoresConversationWithoutCWDFallback(t *testing.T) {
	root := t.TempDir()
	workspaceRoot := t.TempDir()
	secrets := &commandSessionSecrets{}
	model := &commandMatrixModel{}
	preset := config.DefaultAgentConfig("ollama")
	configStore, credentialStore := pairedStores(config.DefaultServerURL, "server-token")
	configStore.value.Agent = &preset
	seedUI := &commandMatrixUI{send: "历史workspace中的对话"}
	seedApp, _, seedErr := newSessionCommandApp(t, configStore, credentialStore, seedUI, model, root, secrets, &fakeTerminal{})
	if exit := seedApp.Run(t.Context(), []string{"agent", "--workspace", workspaceRoot}); exit != ExitOK || seedUI.sendErr != nil {
		t.Fatalf("seed exit=%d send=%v err=%q", exit, seedUI.sendErr, seedErr.String())
	}
	if err := os.RemoveAll(workspaceRoot); err != nil {
		t.Fatal(err)
	}

	resumeConfig, resumeCredentials := pairedStores(config.DefaultServerURL, "server-token")
	resumeConfig.value.Agent = &preset
	resumeUI := &commandMatrixUI{send: "缺失workspace后继续"}
	resumeApp, _, resumeErr := newSessionCommandApp(t, resumeConfig, resumeCredentials, resumeUI, model, root, secrets, &fakeTerminal{confirmed: true})
	if exit := resumeApp.Run(t.Context(), []string{"agent", "resume", seedUI.sessionID, "--all"}); exit != ExitOK || resumeUI.sendErr != nil {
		t.Fatalf("resume exit=%d send=%v err=%q", exit, resumeUI.sendErr, resumeErr.String())
	}
	if resumeUI.workspace.Available || resumeUI.workspace.Code != "workspace_unavailable" {
		t.Fatalf("workspace status=%+v; current cwd must not be used as fallback", resumeUI.workspace)
	}
	if !resumeUI.transcriptContains("历史workspace中的对话") || !model.requestContainsUsers("历史workspace中的对话", "缺失workspace后继续") {
		t.Fatalf("conversation was not restored transcript=%+v requests=%+v", resumeUI.transcript.Entries, model.snapshot())
	}
	if !containsText(resumeErr.String(), "session_workspace_unavailable") {
		t.Fatalf("missing workspace notice=%q", resumeErr.String())
	}
}

func TestAgentCommandProviderChangeZeroSendUntilConfirmationAndLocalManagement(t *testing.T) {
	root := t.TempDir()
	secrets := &commandSessionSecrets{}
	model := &commandMatrixModel{}
	original := config.DefaultAgentConfig("custom")
	original.BaseURL = "http://127.0.0.1:11434/v1"
	original.Model = "old-model"
	seedConfig, seedCredentials := pairedStores(config.DefaultServerURL, "server-token")
	seedConfig.value.Agent = &original
	seedUI := &commandMatrixUI{send: "不得在确认前泄漏的历史"}
	seedApp, _, seedErr := newSessionCommandApp(t, seedConfig, seedCredentials, seedUI, model, root, secrets, &fakeTerminal{})
	if exit := seedApp.Run(t.Context(), []string{"agent"}); exit != ExitOK || seedUI.sendErr != nil {
		t.Fatalf("seed exit=%d send=%v err=%q", exit, seedUI.sendErr, seedErr.String())
	}

	changed := original
	changed.BaseURL = "http://127.0.0.1:2244/v1"
	changed.Model = "new-model"
	declineConfig, declineCredentials := pairedStores(config.DefaultServerURL, "server-token")
	declineConfig.value.Agent = &changed
	declineUI := &commandMatrixUI{rename: "本地重命名仍可用"}
	model.reset()
	declineApp, _, declineErr := newSessionCommandApp(t, declineConfig, declineCredentials, declineUI, model, root, secrets, &fakeTerminal{confirmed: false})
	if exit := declineApp.Run(t.Context(), []string{"agent", "resume", seedUI.sessionID, "--all"}); exit != ExitOK || declineUI.renameErr != nil || declineUI.listErr != nil {
		t.Fatalf("decline exit=%d list=%v rename=%v err=%q", exit, declineUI.listErr, declineUI.renameErr, declineErr.String())
	}
	if got := len(model.snapshot()); got != 0 {
		t.Fatalf("provider decline sent %d model requests: %+v", got, model.snapshot())
	}
	if declineUI.renamedTitle != "本地重命名仍可用" {
		t.Fatalf("local management did not complete: %q", declineUI.renamedTitle)
	}

	confirmConfig, confirmCredentials := pairedStores(config.DefaultServerURL, "server-token")
	confirmConfig.value.Agent = &changed
	confirmUI := &commandMatrixUI{send: "确认后才发送"}
	confirmApp, _, confirmErr := newSessionCommandApp(t, confirmConfig, confirmCredentials, confirmUI, model, root, secrets, &fakeTerminal{confirmed: true})
	if exit := confirmApp.Run(t.Context(), []string{"agent", "resume", seedUI.sessionID, "--all"}); exit != ExitOK || confirmUI.sendErr != nil {
		t.Fatalf("confirm exit=%d send=%v err=%q", exit, confirmUI.sendErr, confirmErr.String())
	}
	if !model.requestContainsUsers("不得在确认前泄漏的历史", "确认后才发送") {
		t.Fatalf("confirmed request lacks history: %+v", model.snapshot())
	}
}

func TestSessionCommandRecoveryGuidanceIsFailClosed(t *testing.T) {
	version := sessionCommandErrorForCode("session_version_unsupported")
	if version == nil || version.ExitCode != ExitConflict || !containsText(version.Next, "升级客户端后重试") || !containsText(version.Next, "当前版本不会恢复、修改或删除") || containsText(version.Next, "删除不兼容Session") {
		t.Fatalf("future-version guidance=%+v", version)
	}
	deleted := sessionCommandErrorForCode("session_delete_failed")
	if deleted == nil || deleted.ExitCode != ExitUnavailable || !containsText(deleted.Detail, "仍按未删除处理") || !containsText(deleted.Next, "不要假定任何数据已删除") {
		t.Fatalf("delete-failure guidance=%+v", deleted)
	}
}

type unavailableCommandSecrets struct{}

func (unavailableCommandSecrets) Available(keybackend.Locator) error {
	return errors.New("native unavailable")
}
func (unavailableCommandSecrets) Load(keybackend.Locator, int) ([]byte, error) {
	return nil, errors.New("unexpected load")
}
func (unavailableCommandSecrets) Store(keybackend.Locator, []byte) error {
	return errors.New("unexpected store")
}
func (unavailableCommandSecrets) Delete(keybackend.Locator) error {
	return errors.New("unexpected delete")
}

type commandMatrixModel struct {
	mu       sync.Mutex
	requests []modelclient.Request
}

func (m *commandMatrixModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	m.mu.Lock()
	m.requests = append(m.requests, request)
	m.mu.Unlock()
	if len(request.Messages) > 0 && containsText(request.Messages[0].Content, "生成简洁自然的中文会话标题") {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: `{"title":"边界测试"}`}}, nil
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "模型回答"}}, nil
}

func (m *commandMatrixModel) snapshot() []modelclient.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]modelclient.Request(nil), m.requests...)
}

func (m *commandMatrixModel) reset() {
	m.mu.Lock()
	m.requests = nil
	m.mu.Unlock()
}

func (m *commandMatrixModel) requestContainsUsers(wanted ...string) bool {
	for _, request := range m.snapshot() {
		found := make(map[string]bool, len(wanted))
		for _, message := range request.Messages {
			if message.Role != "user" {
				continue
			}
			for _, value := range wanted {
				if message.Content == value {
					found[value] = true
				}
			}
		}
		if len(found) == len(wanted) {
			return true
		}
	}
	return false
}

type commandMatrixUI struct {
	send              string
	rename            string
	calls             int
	sessionID         string
	sendErr           error
	listErr           error
	renameErr         error
	renamedTitle      string
	persistenceState  string
	persistenceDetail string
	workspace         agentloop.WorkspaceStatus
	transcript        agentsession.TranscriptV1
}

func (u *commandMatrixUI) Run(ctx context.Context, conversation agentui.Conversation, _ string) error {
	u.calls++
	u.workspace = conversation.WorkspaceStatus()
	if provider, ok := conversation.(interface{ SessionPersistenceStatus() (string, string) }); ok {
		u.persistenceState, u.persistenceDetail = provider.SessionPersistenceStatus()
	}
	if provider, ok := conversation.(interface{ SessionID() string }); ok {
		u.sessionID = provider.SessionID()
	}
	if provider, ok := conversation.(interface {
		SessionTranscript() agentsession.TranscriptV1
	}); ok {
		u.transcript = provider.SessionTranscript()
	}
	if u.rename != "" {
		manager, ok := conversation.(interface {
			ListSessions(context.Context, agentcontroller.SessionListRequest) ([]agentcontroller.SessionListItem, error)
			RenameSession(context.Context, string, string, uint64) (agentsession.Summary, error)
		})
		if !ok {
			u.listErr = errors.New("conversation lacks local session management")
			return nil
		}
		items, err := manager.ListSessions(ctx, agentcontroller.SessionListRequest{All: true})
		u.listErr = err
		if err == nil {
			for _, item := range items {
				if item.Summary.SessionID != u.sessionID {
					continue
				}
				renamed, renameErr := manager.RenameSession(ctx, u.sessionID, u.rename, item.Summary.RecordRevision)
				u.renameErr, u.renamedTitle = renameErr, renamed.Title
				break
			}
		}
	}
	if u.send != "" {
		_, u.sendErr = conversation.Send(ctx, u.send)
	}
	return nil
}

func (u *commandMatrixUI) transcriptContains(wanted string) bool {
	for _, entry := range u.transcript.Entries {
		if entry.Text == wanted {
			return true
		}
	}
	return false
}

func newSessionCommandApp(t *testing.T, configStore ConfigStore, credentialStore CredentialStore, ui AgentUIRunner, model agentloop.Model, root string, secrets agentsession.SecretBackend, terminal Terminal) (*App, *strings.Builder, *strings.Builder) {
	t.Helper()
	app, byteOut, byteErr := newTestApp(configStore, credentialStore, terminal)
	app.ModelSecrets = &memoryModelSecretStore{}
	app.AgentUI = ui
	app.NewModel = func(config.AgentConfig, string) (agentloop.Model, error) { return model, nil }
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }
	app.Getenv = func(string) string { return "xterm" }
	app.AgentSessionRoot = func() (string, error) { return root, nil }
	app.AgentSessionSecrets = secrets
	out, errOut := &strings.Builder{}, &strings.Builder{}
	app.Out, app.Err = out, errOut
	_ = byteOut
	_ = byteErr
	return app, out, errOut
}

func assertNoPlaintextInTree(t *testing.T, root, forbidden string) {
	t.Helper()
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), forbidden) {
			return fmt.Errorf("plaintext found in %s", path)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
}

func containsCommandCode(err error, code string) bool {
	value, ok := err.(*Error)
	return ok && value.Code == code
}

func containsText(value, wanted string) bool {
	return len(value) >= len(wanted) && (value == wanted || stringContains(value, wanted))
}

func stringContains(value, wanted string) bool {
	for index := 0; index+len(wanted) <= len(value); index++ {
		if value[index:index+len(wanted)] == wanted {
			return true
		}
	}
	return false
}
