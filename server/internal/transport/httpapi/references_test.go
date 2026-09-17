package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/integrations/learningknowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningchange"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacedb "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/research"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

func TestPostgreSQLReferencesCookieImportCollectionAndScope(t *testing.T) {
	pool := webTestPool(t)
	ctx := context.Background()
	ids, err := identity.NewService(identitydb.New(pool), identity.Options{PairingCodeTTL: time.Minute, PairingCodeMaxAttempts: 5, LastUsedTouchInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := ids.CreatePairingCodeForProfile(ctx, identity.PairingProfileReferences)
	if err != nil {
		t.Fatal(err)
	}
	cookie, principal, err := ids.ExchangeWebPairing(ctx, code, "浏览器参考验收")
	if err != nil {
		t.Fatal(err)
	}
	kstore := knowledgedb.New(pool)
	ks, err := knowledge.NewService(kstore, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lstore := learningdb.New(pool, tutoringdb.New(pool), kstore)
	ls, err := learning.NewService(lstore, lstore, learningknowledge.New(ks), learning.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	changes, err := learningchange.New(pool, lstore, kstore, nil, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	goal := uuid.NewString()
	_, err = ls.CreateGoal(ctx, principal.Device.ID, learning.GoalCommand{Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "goal", AggregateID: goal, Payload: json.RawMessage(`{}`)}, GoalID: goal, Text: "学习参考资料", Source: "浏览器验收", Details: &learning.GoalDetails{Name: "参考目标"}})
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/office" {
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write([]byte("%PDF 未实现格式"))
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("公共网页原文，不能以摘要替代。"))
	}))
	defer upstream.Close()
	fetcher := research.NewFetcherWithNetwork(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	})
	server := httptest.NewUnstartedServer(nil)
	origin := "http://" + server.Listener.Addr().String()
	base, _ := url.Parse(origin)
	h, err := New(Options{Identity: ids, Knowledge: ks, Learning: ls, LearningChanges: changes, LearningSpaces: spacedb.New(pool), ReferenceFetcher: fetcher, Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PairLimiter: NewFixedWindowLimiter(1000, time.Minute), AuthLimiter: NewFixedWindowLimiter(1000, time.Minute), DeviceLimiter: NewFixedWindowLimiter(1000, time.Minute), WebUI: WebUIOptions{Enabled: true, AllowLoopbackHTTP: true, PublicBaseURL: base, Identity: ids, Assets: fstest.MapFS{"index.html": {Data: []byte(`<script type="module" src="/app/assets/main.js"></script>`)}, "assets/main.js": {Data: []byte(`export {}`)}}}})
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = h
	server.Start()
	defer server.Close()
	call := func(method, path, space, collection string, body any, csrf bool, want int) []byte {
		t.Helper()
		raw, _ := json.Marshal(body)
		r, _ := http.NewRequest(method, origin+path, bytes.NewReader(raw))
		r.AddCookie(&http.Cookie{Name: "edu_web_dev", Value: cookie})
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		if space != "" {
			r.Header.Set(learningspace.Header, space)
		}
		if collection != "" {
			r.Header.Set(knowledge.CollectionHeader, collection)
		}
		if csrf {
			r.Header.Set("X-CSRF-Token", webCSRF(cookie))
		}
		res, e := server.Client().Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		if res.StatusCode != want {
			t.Fatalf("%s %s: %d，预期 %d：%s", method, path, res.StatusCode, want, data)
		}
		return data
	}
	space := learningspace.DefaultID
	data := call("GET", "/v1/web/session", "", "", nil, true, 200)
	if !bytes.Contains(data, []byte(`"references":true`)) {
		t.Fatal("参考配对没有真实能力")
	}
	collection := uuid.NewString()
	create := knowledge.CollectionCommand{ID: collection, Action: "create", Name: "资料集合", Source: "授权选择"}
	call("POST", "/v1/knowledge/collections", space, "", create, false, 403)
	var c knowledge.Collection
	if err = json.Unmarshal(call("POST", "/v1/knowledge/collections", space, "", create, true, 200), &c); err != nil {
		t.Fatal(err)
	}
	call("POST", "/v1/knowledge/collections", space, "", knowledge.CollectionCommand{ID: collection, Action: "edit", Name: "修改集合", Source: "浏览器文件", ExpectedVersion: c.Version}, true, 200)
	sourceData := call("POST", "/v1/knowledge/reference-sources", space, "", map[string]any{"url": "http://example.org/text", "external_consent": true}, true, 200)
	var source research.Source
	if err = json.Unmarshal(sourceData, &source); err != nil || source.Status != "parsed" || !strings.Contains(source.Text, "公共网页原文") {
		t.Fatal("网页未真实解析", err, string(sourceData))
	}
	call("POST", "/v1/knowledge/reference-sources", space, "", map[string]any{"url": "http://127.0.0.1/private", "external_consent": true}, true, 400)
	unsupported := call("POST", "/v1/knowledge/reference-sources", space, "", map[string]any{"url": "http://example.org/office", "external_consent": true}, true, 200)
	if !bytes.Contains(unsupported, []byte("unsupported")) {
		t.Fatal("未实现格式被算作成功", string(unsupported))
	}
	request := knowledge.ImportCommand{OperationID: uuid.NewString(), ExpectedParentProvided: true, Source: "单批真实内容", Documents: []knowledge.ImportDocument{{Path: "markdown.md", Markdown: "# 第一章\n\n教学原文\n\n## 第二章\n\n另一章节"}, {Path: "utf8.md", Markdown: "# UTF-8\n\n```text\n纯文本输入\n```"}, {Path: "paste.md", Markdown: "# 粘贴\n\n实际粘贴原文"}, {Path: "web.md", Markdown: "# 网页\n\n" + source.Text}}}
	var preview knowledge.ImportPreview
	if err = json.Unmarshal(call("POST", "/v1/knowledge/imports/previews", space, collection, request, true, 200), &preview); err != nil || preview.Status != "ready" || preview.Summary.Added != 4 {
		t.Fatal("预览不正确", err, preview)
	}
	cctx, _ := knowledge.WithCollection(ctx, collection)
	if head, e := ks.Head(cctx); e != nil || head != nil {
		t.Fatal("预览入库", e)
	}
	confirm := knowledge.ConfirmImportCommand{Request: request, Receipt: preview.Receipt}
	var result knowledge.ImportResult
	if err = json.Unmarshal(call("POST", "/v1/knowledge/imports/confirm", space, collection, confirm, true, 200), &result); err != nil || result.Summary == nil || result.Summary.Added != 4 {
		t.Fatal("确认结果", err)
	}
	call("POST", "/v1/knowledge/imports/confirm", space, collection, confirm, true, 200)
	call("GET", "/v1/knowledge/imports/operations/"+request.OperationID, space, collection, nil, true, 200)
	call("GET", "/v1/knowledge/revisions/"+result.Revision.ID+"/export", space, collection, nil, true, 200)
	tree, e := ks.Tree(cctx, result.Revision.ID)
	if e != nil {
		t.Fatal(e)
	}
	doc := tree.Revision.Documents[0]
	entry := knowledge.ReferenceEntry{ScopeEntry: knowledge.ScopeEntry{CollectionID: collection, RevisionID: result.Revision.ID, DocumentID: doc.Revision.DocumentID, NodeID: doc.Revision.Nodes[len(doc.Revision.Nodes)-1].NodeID}, Role: "restrict"}
	adoption := learningchange.ReferenceRequest{OperationID: uuid.NewString(), ExpectedGoalVersion: 1, Selection: knowledge.ReferenceSelection{Entries: []knowledge.ReferenceEntry{entry}}}
	path := "/v1/learning/goals/" + goal + "/references"
	var rp learningchange.ReferencePreview
	if err = json.Unmarshal(call("POST", path+"/previews", space, "", adoption, true, 200), &rp); err != nil || !rp.RequiresScopeConfirmation {
		t.Fatal("缺少具体限制确认", err)
	}
	ac := learningchange.ReferenceConfirmation{Request: adoption, Receipt: rp.Receipt}
	call("POST", path+"/confirm", space, "", ac, true, 409)
	ac.ConfirmScope = true
	var state knowledge.ReferenceState
	if err = json.Unmarshal(call("POST", path+"/confirm", space, "", ac, true, 200), &state); err != nil {
		t.Fatal(err)
	}
	call("GET", path, space, "", nil, true, 200)
	call("GET", path+"/operations/"+adoption.OperationID, space, "", nil, true, 200)
	call("POST", "/v1/knowledge/collections", space, "", knowledge.CollectionCommand{ID: collection, Action: "unlink"}, true, 200)
	call("GET", "/v1/knowledge/scopes/"+state.ScopeSnapshotID+"/export", space, "", nil, true, 200)
	adoption.OperationID = uuid.NewString()
	adoption.Selection.Entries = []knowledge.ReferenceEntry{}
	if err = json.Unmarshal(call("POST", path+"/previews", space, "", adoption, true, 200), &rp); err != nil || !rp.RequiresScopeConfirmation {
		t.Fatal("移除限制未确认", err)
	}
	if err = json.Unmarshal(call("POST", path+"/confirm", space, "", learningchange.ReferenceConfirmation{Request: adoption, Receipt: rp.Receipt, ConfirmScope: true}, true, 200), &state); err != nil {
		t.Fatal(err)
	}
	empty := call("GET", "/v1/knowledge/scopes/"+state.ScopeSnapshotID+"/export", space, "", nil, true, 200)
	if !bytes.Contains(empty, []byte(`"documents":[]`)) {
		t.Fatal("未恢复空参考合同", string(empty))
	}
	var sessions int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM tutoring_sessions`).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatal("导入或采用隐式开课", sessions, err)
	}
}
