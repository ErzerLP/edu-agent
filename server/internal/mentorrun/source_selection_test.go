package mentorrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/platform/keyrotation"
	"github.com/google/uuid"
)

func selectedDocument(t *testing.T, f *runtimeFixture, text string) knowledge.SourceIdentity {
	t.Helper()
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE device_tokens SET scopes=array_append(scopes,'knowledge:read') WHERE id=$1`, f.actor.TokenID); err != nil {
		t.Fatal(err)
	}
	store := knowledgepg.New(f.pool)
	ks, err := knowledge.NewService(store, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	collection := uuid.NewString()
	if _, err = ks.ChangeCollection(ctx, knowledge.CollectionCommand{ID: collection, Action: "create", Name: "同名集合", Source: "来源选择验收"}); err != nil {
		t.Fatal(err)
	}
	ctx, _ = knowledge.WithCollection(ctx, collection)
	request := knowledge.ImportCommand{OperationID: uuid.NewString(), ActorDeviceID: f.actor.Device.ID, ExpectedParentProvided: true, Source: "用户文本", Documents: []knowledge.ImportDocument{{Path: "same.md", Markdown: "# 标题 " + text + "\n\n" + text + "\n"}}}
	preview, err := ks.PreviewImport(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	imported, err := ks.ConfirmImport(ctx, knowledge.ConfirmImportCommand{Request: request, Receipt: preview.Receipt})
	if err != nil {
		t.Fatal(err)
	}
	d := imported.Revision.Documents[0].Revision
	return knowledge.SourceIdentity{CollectionID: collection, RevisionID: imported.Revision.ID, DocumentID: d.DocumentID, DocumentRevisionID: d.ID}
}

func TestSourceSelectionValidationAndEncryptedCompatibility(t *testing.T) {
	for _, s := range []knowledge.SourceSelection{{}, {Mode: "none"}, {Mode: "items", Items: []knowledge.SourceIdentity{}}, {Mode: "all", Items: []knowledge.SourceIdentity{}}, {Mode: "items", Items: []knowledge.SourceIdentity{{CollectionID: uuid.NewString()}}}} {
		if s.Validate() == nil {
			t.Fatalf("accepted invalid selection %+v", s)
		}
	}
	none := &knowledge.SourceSelection{Mode: "none", Items: []knowledge.SourceIdentity{}}
	if none.Validate() != nil {
		t.Fatal("rejected explicit none")
	}
	if requestHash("create", "space", "goal", Create{}) == requestHash("create", "space", "goal", Create{SourceSelection: none}) {
		t.Fatal("none confused with absent")
	}
	cipher, _ := newCipher(bytes.Repeat([]byte{42}, 32))
	manifest := &knowledge.SourceManifest{Schema: 1, SpaceID: learningspace.DefaultID, Generation: 1, Entries: []knowledge.SourceManifestEntry{}}
	manifest.SHA256 = manifest.Hash()
	for _, body := range []Body{{Output: "legacy"}, {SourceManifest: manifest, Output: "selected"}} {
		raw, err := seal(cipher, "run", body)
		if err != nil {
			t.Fatal(err)
		}
		got, err := unseal(cipher, "run", raw)
		if err != nil || !reflect.DeepEqual(got, body) {
			t.Fatal("checkpoint compatibility", err)
		}
		raw, err = sealHistory(cipher, "history", body)
		if err != nil {
			t.Fatal(err)
		}
		var history Body
		if err = openHistory(cipher, "history", raw, &history); err != nil || !reflect.DeepEqual(history, body) {
			t.Fatal("history compatibility", err)
		}
	}
	for _, name := range []string{"read_goal", "read_learning_context", "read_learning_progress", "open_references", "request_memory", "local_operation", "propose_learning_change"} {
		host := executionHost{body: Body{SourceManifest: manifest}}
		if _, err := host.Execute(context.Background(), []modelclient.ToolCall{{ID: "forged", Function: modelclient.ToolFunction{Name: name, Arguments: `{}`}}}); !errors.Is(err, ErrInvalid) {
			t.Fatal("model bypassed selected tool allowlist", name, err)
		}
	}
}

func TestSourceSelectionCitationRejectsChangedIdentityRangeAndHash(t *testing.T) {
	source := knowledge.SourceItem{CollectionID: uuid.NewString(), RevisionID: uuid.NewString(), DocumentID: uuid.NewString(), DocumentRevisionID: uuid.NewString()}
	fragment := knowledge.SourceFragment{NodeID: uuid.NewString(), NodeRevisionID: uuid.NewString(), Text: "原文", Range: knowledge.SourceRange{Start: 3, End: 9, StartLine: 2, EndLine: 2}}
	manifest := &knowledge.SourceManifest{Entries: []knowledge.SourceManifestEntry{{SHA256: knowledge.SourceTextHash("完整原文")}}}
	citation := SourceCitation{ID: "1", Source: source, Fragment: fragment, DocumentSHA256: manifest.Entries[0].SHA256, SliceSHA256: knowledge.SourceTextHash(fragment.Text)}
	details := []knowledge.SourceDetail{{SourceItem: source, Fragments: []knowledge.SourceFragment{fragment}}}
	if err := verifyStoredCitations(Body{SourceManifest: manifest, Citations: []SourceCitation{citation}}, details); err != nil {
		t.Fatal(err)
	}
	for name, corrupt := range map[string]func(*SourceCitation){
		"collection":        func(c *SourceCitation) { c.Source.CollectionID = uuid.NewString() },
		"revision":          func(c *SourceCitation) { c.Source.RevisionID = uuid.NewString() },
		"document":          func(c *SourceCitation) { c.Source.DocumentID = uuid.NewString() },
		"document revision": func(c *SourceCitation) { c.Source.DocumentRevisionID = uuid.NewString() },
		"node revision":     func(c *SourceCitation) { c.Fragment.NodeRevisionID = uuid.NewString() },
		"range":             func(c *SourceCitation) { c.Fragment.Range.Start++ },
		"line range":        func(c *SourceCitation) { c.Fragment.Range.StartLine++ },
		"text":              func(c *SourceCitation) { c.Fragment.Text = "伪造" },
		"document hash":     func(c *SourceCitation) { c.DocumentSHA256 = knowledge.SourceTextHash("其他全文") },
		"slice hash":        func(c *SourceCitation) { c.SliceSHA256 = knowledge.SourceTextHash("其他片段") },
	} {
		t.Run(name, func(t *testing.T) {
			forged := citation
			corrupt(&forged)
			if err := verifyStoredCitations(Body{SourceManifest: manifest, Citations: []SourceCitation{forged}}, details); !errors.Is(err, errCitation) {
				t.Fatal("accepted forged citation", err)
			}
		})
	}
}

func TestPostgreSQLSourceSelectionForgeryRevocationAndRotation(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		stream(w, modelclient.Message{Role: "assistant", Content: `{"answer":"原文 [1]","citations":[{"source":1,"fragment":1}]}`})
	})
	ctx := context.Background()
	a, b := selectedDocument(t, f, "AUTHORIZED"), selectedDocument(t, f, "OTHER_COLLECTION")
	other := uuid.NewString()
	if _, err := f.pool.Exec(ctx, `INSERT INTO learning_spaces(id,name,status,version) VALUES($1,'another','active',1)`, other); err != nil {
		t.Fatal(err)
	}
	conversation, err := f.service.NewConversation(ctx, f.actor, other, NewConversation{ID: uuid.NewString(), Saved: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.Submit(ctx, f.actor, other, conversation, SubmitTurn{OperationID: uuid.NewString(), ExpectedVersion: 1, Prompt: "forged cross space", RequestBudget: 3, TokenBudget: 30000, SourceSelection: &knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{a}}}); knowledge.ErrorCode(err) != knowledge.CodeNotFound {
		t.Fatal("cross space selection accepted", err)
	}
	id := newHistory(t, f, true, "")
	for _, bad := range []knowledge.SourceIdentity{{CollectionID: b.CollectionID, RevisionID: a.RevisionID, DocumentID: a.DocumentID, DocumentRevisionID: a.DocumentRevisionID}, {CollectionID: a.CollectionID, RevisionID: a.RevisionID, DocumentID: b.DocumentID, DocumentRevisionID: a.DocumentRevisionID}} {
		req := historyRequest(t, f, id, "不应受理")
		req.SourceSelection = &knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{bad}}
		if _, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, uuid.Nil.String(), req); knowledge.ErrorCode(err) != knowledge.CodeNotFound {
			t.Fatal("forgery accepted", err)
		}
	}
	request := historyRequest(t, f, id, "读取并保存")
	request.SourceSelection = &knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{a}}
	receipt, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, uuid.Nil.String(), request)
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	before := f.snapshot(t, receipt.RunID)
	rewrap, _ := keyrotation.New(bytes.Repeat([]byte{42}, 32), bytes.Repeat([]byte{43}, 32))
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `LOCK TABLE learning_mentor_runs,learning_tutor_conversations,learning_tutor_turns IN ACCESS EXCLUSIVE MODE; SET LOCAL app.mentor_key_rotation='on'`); err != nil {
		t.Fatal(err)
	}
	if err = RotateKeyTx(ctx, tx, rewrap); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	f.service, err = New(f.pool, f.settings, bytes.Repeat([]byte{43}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if after := f.snapshot(t, receipt.RunID); !reflect.DeepEqual(before.Citations, after.Citations) || !reflect.DeepEqual(before.SourceManifest, after.SourceManifest) {
		t.Fatal("rotation lost selected source body")
	}
	if _, err = f.pool.Exec(ctx, `UPDATE learning_mentor_runs SET expires_at=clock_timestamp()-interval '1 second',state=jsonb_set(state,'{expires_at}',to_jsonb(clock_timestamp()-interval '1 second')) WHERE id=$1`, receipt.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if err = f.service.ReadCitation(ctx, f.actor, learningspace.DefaultID, receipt.RunID, "1", func(c SourceCitation) error {
		if c.Fragment.Text != before.Citations[0].Fragment.Text {
			t.Error("expired citation changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if page := historyPage(t, f, id, 0, 20); len(page.Items[0].Citations) != 1 {
		t.Fatal("history lost rotated citations")
	}
	// Removing knowledge read permission hides every saved derivative, not just its link.
	if _, err = f.pool.Exec(ctx, `UPDATE device_tokens SET scopes=array_remove(scopes,'knowledge:read') WHERE id=$1`, f.actor.TokenID); err != nil {
		t.Fatal(err)
	}
	if page := historyPage(t, f, id, 0, 20); page.Items[0].BodyAvailable || page.Items[0].Output != "" || len(page.Items[0].Messages) > 0 {
		t.Fatal("revoked history leaked")
	}
	if err = f.service.ReadCitation(ctx, f.actor, learningspace.DefaultID, receipt.RunID, "1", func(SourceCitation) error { t.Error("revoked citation sent"); return nil }); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(ctx, `UPDATE device_tokens SET scopes=array_append(scopes,'knowledge:read') WHERE id=$1`, f.actor.TokenID); err != nil {
		t.Fatal(err)
	}
	store := knowledgepg.New(f.pool)
	if _, err = store.ChangeCollection(ctx, knowledge.CollectionCommand{ID: a.CollectionID, Action: "unlink"}); err != nil {
		t.Fatal(err)
	}
	if page := historyPage(t, f, id, 0, 20); page.Items[0].BodyAvailable {
		t.Fatal("unlinked history leaked")
	}
	if err = f.service.ReadCitation(ctx, f.actor, learningspace.DefaultID, receipt.RunID, "1", func(SourceCitation) error { t.Error("unlinked citation sent"); return nil }); knowledge.ErrorCode(err) != knowledge.CodeNotFound {
		t.Fatal(err)
	}
}

func TestPostgreSQLSourceSelectionPausedRunPinsHistoricalVersion(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		var payload modelclient.Request
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		raw, _ := json.Marshal(payload)
		if !bytes.Contains(raw, []byte("HISTORY_OLD_BODY")) || bytes.Contains(raw, []byte("HISTORY_NEW_BODY")) {
			t.Errorf("historical run widened to head: %s", raw)
		}
		stream(w, modelclient.Message{Role: "assistant", Content: `{"answer":"历史原文 [1]","citations":[{"source":1,"fragment":1}]}`})
	})
	ctx := context.Background()
	a := selectedDocument(t, f, "HISTORY_OLD_BODY")
	id := newHistory(t, f, true, "")
	req := historyRequest(t, f, id, "保存当时的版本")
	req.TokenBudget = 1
	req.SourceSelection = &knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{a}}
	receipt, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, uuid.Nil.String(), req)
	if err != nil {
		t.Fatal(err)
	}
	legacyPause(t, f, receipt.RunID)
	f.work(t)
	before := f.snapshot(t, receipt.RunID)
	if before.Status != "paused_budget" || f.calls.Load() != 0 {
		t.Fatal("did not pause before request")
	}
	ks, err := knowledge.NewService(knowledgepg.New(f.pool), knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scoped, _ := knowledge.WithCollection(ctx, a.CollectionID)
	update := knowledge.ImportCommand{OperationID: uuid.NewString(), ActorDeviceID: f.actor.Device.ID, ExpectedParentProvided: true, ExpectedParentRevisionID: &a.RevisionID, Source: "update", Documents: []knowledge.ImportDocument{{Path: "same.md", Markdown: "# 标题 HISTORY_OLD_BODY\n\nHISTORY_NEW_BODY\n"}}}
	preview, err := ks.PreviewImport(scoped, update)
	if err != nil {
		t.Fatal(err)
	}
	for step := 0; preview.Review != nil && step < 3; step++ {
		update.OperationID = uuid.NewString()
		update.IdentityReviewBasisHash, update.IdentityReviewOperationID, update.IdentityReviewReceipt = preview.Review.BasisHash, preview.Review.OperationID, preview.Review.Receipt
		for _, d := range preview.Review.Documents {
			update.DocumentResolutions = append(update.DocumentResolutions, knowledge.DocumentResolution{Locator: d.Locator, Action: "preserve", DocumentID: a.DocumentID, Reason: "用户确认原文更新"})
		}
		for _, n := range preview.Review.Nodes {
			update.NodeResolutions = append(update.NodeResolutions, knowledge.NodeResolution{Locator: n.Locator, Action: "rewrite", SourceNodeRevisionIDs: []string{n.Candidates[0].RevisionID}, Reason: "更新原文"})
		}
		preview, err = ks.PreviewImport(scoped, update)
		if err != nil {
			t.Fatal(err)
		}
	}
	if preview.Status != "ready" {
		t.Fatalf("fixture identity review incomplete: %+v", preview)
	}
	if _, err = ks.ConfirmImport(scoped, knowledge.ConfirmImportCommand{Request: update, Receipt: preview.Receipt}); err != nil {
		t.Fatal(err)
	}
	if replay, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, uuid.Nil.String(), req); err != nil || replay != receipt {
		t.Fatal("changed head broke original replay", err)
	}
	if _, err = f.service.Command(ctx, f.actor, learningspace.DefaultID, receipt.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: before.Version, Kind: "continue_budget", RequestBudget: 3, TokenBudget: 30000}); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	after := f.snapshot(t, receipt.RunID)
	if after.Status != "succeeded" || after.SourceManifest.SHA256 != before.SourceManifest.SHA256 || after.Citations[0].Source.RevisionID != a.RevisionID {
		t.Fatalf("historical selection changed: %+v", after)
	}
}

func TestPostgreSQLSourceSelectionDoesNotChangeFrozenClassroom(t *testing.T) {
	f, _, entry := referenceFixture(t)
	old := f.context(t)
	ctx, _ := knowledge.WithCollection(context.Background(), entry.CollectionID)
	catalog, err := f.service.starter.Knowledge.Sources(ctx, knowledge.SourceQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	source := catalog.Items[0]
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) {
		var payload modelclient.Request
		_ = json.NewDecoder(r.Body).Decode(&payload)
		raw, _ := json.Marshal(payload)
		if bytes.Contains(raw, []byte("真实绑定目标")) {
			t.Error("classroom context entered selected question")
		}
		stream(w, modelclient.Message{Role: "assistant", Content: `{"answer":"本轮参考 [1]","citations":[{"source":1,"fragment":1}]}`})
	}
	request := Create{OperationID: uuid.NewString(), SessionID: uuid.NewString(), ExpectedVersion: old.Base.GoalVersion, TeachingSessionID: f.session, Prompt: "只问所选来源", Save: true, RequestBudget: 3, TokenBudget: 50000, SourceSelection: &knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{{CollectionID: source.CollectionID, RevisionID: source.RevisionID, DocumentID: source.DocumentID, DocumentRevisionID: source.DocumentRevisionID}}}}
	receipt, err := f.service.Create(context.Background(), f.actor, learningspace.DefaultID, f.goal, request)
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	if result := f.snapshot(t, receipt.RunID); result.Status != "succeeded" {
		t.Fatalf("selected teaching question: %+v", result)
	}
	if after := f.context(t); !reflect.DeepEqual(old, after) {
		t.Fatal("selected question changed frozen classroom")
	}
}

func TestPostgreSQLSourceSelectionLegacyHistoryAndInvalidCitations(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			stream(w, modelclient.Message{Role: "assistant", Content: "LEGACY_UNSELECTED_TITLE_AND_BODY"})
			return
		}
		var request modelclient.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		raw, _ := json.Marshal(request)
		if bytes.Contains(raw, []byte("LEGACY_UNSELECTED")) {
			t.Error("legacy conversation leaked outside new selection")
		}
		stream(w, modelclient.Message{Role: "assistant", Content: `{"answer":"伪造引用 [1]","citations":[{"source":99,"fragment":1}]}`})
	})
	id := newHistory(t, f, true, "")
	sendHistory(t, f, id, "LEGACY_UNSELECTED_QUESTION")
	f.work(t)
	c := historyPage(t, f, id, 0, 20).Conversation
	receipt, err := f.service.Submit(context.Background(), f.actor, learningspace.DefaultID, id, SubmitTurn{OperationID: uuid.NewString(), ExpectedVersion: c.Version, Prompt: "新问题", RequestBudget: 3, TokenBudget: 30000, SourceSelection: &knowledge.SourceSelection{Mode: "none", Items: []knowledge.SourceIdentity{}}})
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	snapshot := f.snapshot(t, receipt.RunID)
	if snapshot.Status != "failed" || snapshot.Reason != "invalid_source_citation" || len(snapshot.Citations) > 0 || snapshot.Output != "" {
		t.Fatalf("forged citation exposed: %+v", snapshot)
	}
}

func TestPostgreSQLSourceSelectionClearInFlightCannotRestoreBody(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		stream(w, modelclient.Message{Role: "assistant", Content: `{"answer":"迟到原文 [1]","citations":[{"source":1,"fragment":1}]}`})
	})
	a := selectedDocument(t, f, "CLEAR_PRIVATE_SOURCE")
	id := newHistory(t, f, true, "")
	req := historyRequest(t, f, id, "清除竞态")
	req.SourceSelection = &knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{a}}
	receipt, err := f.service.Create(context.Background(), f.actor, learningspace.DefaultID, uuid.Nil.String(), req)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := f.service.RunOnce(context.Background()); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not start")
	}
	current := f.snapshot(t, receipt.RunID)
	if _, err = f.service.Command(context.Background(), f.actor, learningspace.DefaultID, receipt.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: current.Version, Kind: "clear"}); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not settle")
	}
	if snapshot := f.snapshot(t, receipt.RunID); snapshot.BodyAvailable || snapshot.Output != "" || len(snapshot.Citations) > 0 {
		t.Fatal("late response revived body")
	}
	if page := historyPage(t, f, id, 0, 20); page.Items[0].BodyAvailable {
		t.Fatal("late history revived")
	}
}

func TestPostgreSQLSourceSelectionToolsAndSameScopeHistory(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		var request modelclient.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		raw, _ := json.Marshal(request)
		if !bytes.Contains(raw, []byte("TOOL_SELECTED")) || bytes.Contains(raw, []byte("TOOL_UNSELECTED")) {
			t.Errorf("tool request escaped selection: %s", raw)
		}
		if n > 2 && !bytes.Contains(raw, []byte("FIRST_SAME_SCOPE_QUESTION")) {
			t.Error("same-scope history was lost")
		}
		if n%2 == 1 {
			stream(w, modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: uuid.NewString(), Type: "function", Function: modelclient.ToolFunction{Name: "read_references", Arguments: `{}`}}}})
			return
		}
		last := request.Messages[len(request.Messages)-1]
		if last.Role != "tool" || !strings.Contains(last.Content, "TOOL_SELECTED") || strings.Contains(last.Content, "TOOL_UNSELECTED") {
			t.Errorf("unscoped tool result: %+v", last)
		}
		stream(w, modelclient.Message{Role: "assistant", Content: `{"answer":"工具核验 [1]","citations":[{"source":1,"fragment":1}]}`})
	})
	a := selectedDocument(t, f, "TOOL_SELECTED")
	selectedDocument(t, f, "TOOL_UNSELECTED")
	id := newHistory(t, f, true, "")
	for _, prompt := range []string{"FIRST_SAME_SCOPE_QUESTION", "继续上一轮"} {
		request := historyRequest(t, f, id, prompt)
		request.SourceSelection = &knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{a}}
		receipt, err := f.service.Create(context.Background(), f.actor, learningspace.DefaultID, uuid.Nil.String(), request)
		if err != nil {
			t.Fatal(err)
		}
		f.work(t)
		if snapshot := f.snapshot(t, receipt.RunID); snapshot.Status != "succeeded" || len(snapshot.Citations) != 1 {
			t.Fatalf("tool answer failed: %+v", snapshot)
		}
	}
	if f.calls.Load() != 4 {
		t.Fatal("expected tool and final calls for each turn", f.calls.Load())
	}
}

func TestPostgreSQLSourceSelectionModelHistoryAndCitations(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		var request modelclient.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		raw, _ := json.Marshal(request)
		want, forbidden := "SELECTED_ALPHA", "UNSELECTED_BETA"
		if n == 2 {
			want, forbidden = forbidden, want
		}
		if n == 3 {
			want = "未选来源"
		}
		if !bytes.Contains(raw, []byte(want)) || bytes.Contains(raw, []byte(forbidden)) || (n == 3 && bytes.Contains(raw, []byte("UNSELECTED_BETA"))) {
			t.Errorf("model request escaped selection on call %d: %s", n, raw)
		}
		for _, tool := range request.Tools {
			if tool.Function.Name != "read_references" && tool.Function.Name != "ask_user" && tool.Function.Name != "confirm_focus" {
				t.Errorf("unscoped tool: %s", tool.Function.Name)
			}
		}
		answer := `{"answer":"已核对原文 [1]","citations":[{"source":1,"fragment":1}]}`
		if n == 3 {
			answer = `{"answer":"未选来源，不引用资料","citations":[]}`
		}
		stream(w, modelclient.Message{Role: "assistant", Content: answer})
	})
	a, b := selectedDocument(t, f, "SELECTED_ALPHA"), selectedDocument(t, f, "UNSELECTED_BETA")
	id := newHistory(t, f, true, "")
	for i, selection := range []*knowledge.SourceSelection{{Mode: "items", Items: []knowledge.SourceIdentity{a}}, {Mode: "items", Items: []knowledge.SourceIdentity{b}}, {Mode: "none", Items: []knowledge.SourceIdentity{}}} {
		request := historyRequest(t, f, id, "请根据资料解释")
		request.SourceSelection = selection
		receipt, err := f.service.Create(context.Background(), f.actor, learningspace.DefaultID, uuid.Nil.String(), request)
		if err != nil {
			t.Fatal(err)
		}
		replay, err := f.service.Create(context.Background(), f.actor, learningspace.DefaultID, uuid.Nil.String(), request)
		if err != nil || replay != receipt {
			t.Fatal("operation did not replay", err)
		}
		f.work(t)
		snapshot := f.snapshot(t, receipt.RunID)
		if snapshot.Status != "succeeded" {
			t.Fatalf("run failed: %+v", snapshot)
		}
		if i < 2 {
			if len(snapshot.Citations) != 1 || snapshot.Citations[0].Source.CollectionID != selection.Items[0].CollectionID {
				t.Fatalf("unverified citation: %+v", snapshot)
			}
			err = f.service.ReadCitation(context.Background(), f.actor, learningspace.DefaultID, receipt.RunID, "1", func(c SourceCitation) error {
				if !strings.Contains(c.Fragment.Text, []string{"SELECTED_ALPHA", "UNSELECTED_BETA"}[i]) {
					t.Error("wrong citation text")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		} else if len(snapshot.Citations) != 0 {
			t.Fatal("none selection has citations")
		}
	}
	page := historyPage(t, f, id, 0, 20)
	if len(page.Items) != 3 || len(page.Items[0].Citations) != 1 {
		t.Fatal("history lost citations")
	}
	var goals int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM learning_goal_revisions`).Scan(&goals); err != nil || goals != 1 {
		t.Fatal("source question created a goal", err)
	}
}
