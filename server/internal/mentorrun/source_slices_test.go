package mentorrun

import (
	"context"
	"encoding/json"
	"github.com/edu-agent/edu-agent/packages/agentcore"
	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"net/http"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
)

func TestPostgreSQLSourceSlicesProofAndManifest(t *testing.T) {
	f := fixture(t, func(http.ResponseWriter, *http.Request, int) {})
	source := selectedDocument(t, f, strings.Repeat("正文中文。\n", 1000)+"\n## 后续章节\n\n尾部事实")
	outside := selectedDocument(t, f, "未选择来源")
	ctx, _ := learningspace.WithScope(context.Background(), learningspace.DefaultID)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	store := knowledgepg.New(f.pool)
	manifest, err := store.FreezeSourceSelectionTx(ctx, tx, knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{source}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.ReadSourceDirectoryTx(ctx, tx, *manifest, "", 1)
	if err != nil || len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatal(first, err)
	}
	next, err := store.ReadSourceDirectoryTx(ctx, tx, *manifest, first.NextCursor, 1)
	if err != nil || len(next.Items) != 1 || next.Items[0].NodeRevisionID == first.Items[0].NodeRevisionID {
		t.Fatal(next, err)
	}
	node := first.Items[0]
	request := knowledge.SourceSliceRequest{SourceIdentity: source, NodeID: node.NodeID, NodeRevisionID: node.NodeRevisionID, Start: node.Range.Start, End: min(node.Range.End, node.Range.Start+300)}
	detail, err := store.ReadSourceSliceTx(ctx, tx, *manifest, request)
	if err != nil || len(detail.Fragments) != 1 {
		t.Fatal(detail, err)
	}
	fragment := detail.Fragments[0]
	if fragment.Proof == nil || fragment.Proof.Schema != 2 || len(fragment.Text) > 300 || !fragment.Truncated {
		t.Fatal("切片无证明", fragment)
	}
	body := Body{SourceManifest: manifest, Citations: []SourceCitation{{ID: "1", Source: detail.SourceItem, Fragment: fragment, DocumentSHA256: manifest.Entries[0].SHA256, SliceSHA256: knowledge.SourceTextHash(fragment.Text)}}}
	checked, err := f.service.sourceMaterialTx(ctx, tx, learningspace.DefaultID, f.actor.TokenID, body)
	if err != nil || verifyStoredCitations(body, checked) != nil {
		t.Fatal("合法切片被拒绝", err)
	}
	body.Citations[0].Fragment.Range.End++
	checked, err = f.service.sourceMaterialTx(ctx, tx, learningspace.DefaultID, f.actor.TokenID, body)
	if err == nil && verifyStoredCitations(body, checked) == nil {
		t.Fatal("篡改区间被接受")
	}
	request.SourceIdentity = outside
	if _, err = store.ReadSourceSliceTx(ctx, tx, *manifest, request); err == nil {
		t.Fatal("跨 manifest 切片被接受")
	}
}

func TestPostgreSQLLargeSelectedSourceRequestIsBounded(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		var request modelclient.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if agentcore.NewTokenEstimator().EstimateRequest(request)+request.MaxTokens+410 > 8192 {
			t.Error("来源请求超过窗口")
		}
		raw, _ := json.Marshal(request.Messages)
		if !strings.Contains(string(raw), "目标章节的事实") {
			t.Error("没有选中相关章节")
		}
		stream(w, modelclient.Message{Role: "assistant", Content: `{"answer":"依据片段回答 [1]","citations":[{"source":1,"fragment":1}]}`})
	})
	var update settings.Update
	_ = json.Unmarshal([]byte(`{"expected_revision":1,"limits":{"context_tokens":8192}}`), &update)
	update.ExpectedRevision = f.settings.View().Revision
	if _, err := f.settings.Update(update); err != nil {
		t.Fatal(err)
	}
	text := ""
	for i := 0; i < 90; i++ {
		text += "\n## 普通章节\n\n" + strings.Repeat("大量来源正文。", 100) + "\n"
	}
	text += "\n## TARGET_TOPIC\n\n目标章节的事实\n"
	source := selectedDocument(t, f, text)
	id := newHistory(t, f, true, "")
	request := historyRequest(t, f, id, "TARGET_TOPIC")
	request.SourceSelection = &knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{source}}
	receipt, err := f.service.Create(context.Background(), f.actor, learningspace.DefaultID, runtimeGoal(""), request)
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	result := f.snapshot(t, receipt.RunID)
	if result.Status != "succeeded" || len(result.Citations) != 1 || result.Citations[0].Fragment.Proof == nil {
		t.Fatal("来源切片未完成引用核验", result)
	}
}
