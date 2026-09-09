package postgresstore_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	space "github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
)

func TestPostgreSQLKnowledgeSpacesIsolationAndFrozenVersions(t *testing.T) {
	ctx, pool, _, s := newReviewerPostgresHarness(t)
	a, b := uuid.NewString(), uuid.NewString()
	for _, id := range []string{a, b} {
		if _, err := pool.Exec(ctx, `INSERT INTO learning_spaces(id,name,status,version) VALUES($1,'测试区','active',1)`, id); err != nil {
			t.Fatal(err)
		}
	}
	spaceA, _ := space.WithScope(ctx, a)
	spaceB, _ := space.WithScope(ctx, b)
	collectionA, collectionB := uuid.NewString(), uuid.NewString()
	for i, c := range []struct {
		ctx context.Context
		id  string
	}{{spaceA, collectionA}, {spaceB, collectionB}} {
		if _, err := s.ChangeCollection(c.ctx, knowledge.CollectionCommand{Action: "create", ID: c.id, Name: []string{"Go 后端", "英语"}[i], Source: []string{"go-repository", "english-notes"}[i]}); err != nil {
			t.Fatal(err)
		}
	}
	ctxA, _ := knowledge.WithCollection(spaceA, collectionA)
	ctxB, _ := knowledge.WithCollection(spaceB, collectionB)
	if _, err := s.ChangeCollection(spaceA, knowledge.CollectionCommand{ID: collectionA, Action: "unlink"}); err != nil {
		t.Fatal(err)
	}
	available, err := s.Collections(spaceA, true)
	if err != nil || len(available) != 1 || available[0].ID != collectionA {
		t.Fatalf("本区私有资料解除后无法重新发现: %+v %v", available, err)
	}
	if _, err := s.ChangeCollection(spaceB, knowledge.CollectionCommand{ID: collectionA, Action: "link"}); knowledge.ErrorCode(err) != knowledge.CodeNotFound {
		t.Fatalf("其他区未共享即可关联: %v", err)
	}
	if _, err := s.ChangeCollection(spaceA, knowledge.CollectionCommand{ID: collectionA, Action: "link"}); err != nil {
		t.Fatalf("创建区无法重新关联: %v", err)
	}
	importDoc := func(ctx context.Context, source, body string, parent *string) knowledge.ImportResult {
		t.Helper()
		result, err := s.Import(ctx, knowledge.ImportCommand{OperationID: uuid.NewString(), ExpectedParentProvided: true, ExpectedParentRevisionID: parent, Source: source, ActorDeviceID: integrationActorID, Documents: []knowledge.ImportDocument{{Path: "README.md", Markdown: body}}})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := importDoc(ctxA, "go-repository", "# ConfidentialAncestor\n\n## Channel\nchannel alpha public learning material has stable words for identity\n\n# Private\nhidden-only beta\n", nil)
	second := importDoc(ctxB, "english-notes", "# English\nenglish-only vocabulary\n", nil)
	if first.Revision.Documents[0].Revision.DocumentID == second.Revision.Documents[0].Revision.DocumentID {
		t.Fatal("不同来源同名文件合并了身份")
	}
	for _, c := range []context.Context{spaceB, ctxB} {
		if _, err := s.Export(c, first.Revision.ID); knowledge.ErrorCode(err) != knowledge.CodeNotFound {
			t.Fatalf("跨区导出未拒绝: %v", err)
		}
	}
	got, err := s.Retrieve(ctxB, knowledge.RetrievalCommand{Query: "channel"})
	if err != nil || len(got.Hits) != 0 {
		t.Fatalf("检索混入另一集合: %+v %v", got, err)
	}
	if _, err = s.ChangeCollection(ctxA, knowledge.CollectionCommand{Action: "share", ID: collectionA, Shared: true, ExpectedVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ChangeCollection(spaceB, knowledge.CollectionCommand{Action: "link", ID: collectionA}); err != nil {
		t.Fatal(err)
	}
	node := first.Revision.Documents[0].Revision.Nodes[2]
	snapshot, err := s.FreezeScope(spaceB, knowledge.ScopeSnapshot{ID: uuid.NewString(), Entries: []knowledge.ScopeEntry{{CollectionID: collectionA, RevisionID: first.Revision.ID, DocumentID: first.Revision.Documents[0].Revision.DocumentID, NodeID: node.NodeID}, {CollectionID: collectionB, RevisionID: second.Revision.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReadScope(spaceA, snapshot.ID); knowledge.ErrorCode(err) != knowledge.CodeNotFound {
		t.Fatalf("跨区快照未拒绝: %v", err)
	}
	canonical := first.Revision.Documents[0].Revision.CanonicalMarkdown
	updated := importDoc(ctxA, "go-repository", strings.Replace(canonical, "alpha public", "gamma public", 1), &first.Revision.ID)
	if updated.Revision.ID == first.Revision.ID {
		t.Fatal("内容更新未生成版本")
	}
	status, err := s.ReadScope(spaceB, snapshot.ID)
	if err != nil || len(status.Updates) != 1 || status.Entries[0].RevisionID != first.Revision.ID {
		t.Fatalf("更新提示修改了冻结范围: %+v %v", status, err)
	}
	if _, err = s.ChangeCollection(spaceB, knowledge.CollectionCommand{Action: "unlink", ID: collectionA}); err != nil {
		t.Fatal(err)
	}
	exported, err := s.Export(spaceB, snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	for _, doc := range exported.Documents {
		body.WriteString(doc.Markdown)
	}
	if !strings.Contains(body.String(), "alpha public") || strings.Contains(body.String(), "gamma public") || strings.Contains(body.String(), "hidden-only") {
		t.Fatalf("冻结范围或章节选择失效: %s", body.String())
	}
	got, err = s.Retrieve(spaceB, knowledge.RetrievalCommand{Query: "hidden", KnowledgeRevisionID: &snapshot.ID})
	if err != nil || len(got.Hits) != 0 {
		t.Fatalf("章节外正文进入候选评分: %+v %v", got, err)
	}
	got, err = s.Retrieve(spaceB, knowledge.RetrievalCommand{Query: "ConfidentialAncestor", ScopeSnapshotID: &snapshot.ID})
	if err != nil || len(got.Hits) != 0 {
		t.Fatalf("范围外祖先标题进入评分: %+v %v", got, err)
	}
	if _, err = s.Export(ctxA, updated.Revision.ID); err != nil {
		t.Fatalf("解除 B 引用影响 A: %v", err)
	}
	var wg sync.WaitGroup
	outcomes := make(chan error, 2)
	for _, word := range []string{"delta", "sigma"} {
		wg.Add(1)
		go func(word string) {
			defer wg.Done()
			_, err := s.Import(ctxA, knowledge.ImportCommand{OperationID: uuid.NewString(), ExpectedParentProvided: true, ExpectedParentRevisionID: &updated.Revision.ID, Source: "go-repository", ActorDeviceID: integrationActorID, Documents: []knowledge.ImportDocument{{Path: "README.md", Markdown: strings.Replace(updated.Revision.Documents[0].Revision.CanonicalMarkdown, "gamma", word, 1)}}})
			outcomes <- err
		}(word)
	}
	wg.Wait()
	close(outcomes)
	success, conflicts := 0, 0
	for err := range outcomes {
		if err == nil {
			success++
		} else if knowledge.ErrorCode(err) == knowledge.CodeRevisionConflict {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("集合并发 CAS 失效: 成功=%d 冲突=%d", success, conflicts)
	}
	if _, err = s.FreezeScope(spaceA, knowledge.ScopeSnapshot{ID: uuid.NewString(), Entries: []knowledge.ScopeEntry{{CollectionID: collectionA, RevisionID: second.Revision.ID}}}); knowledge.ErrorCode(err) != knowledge.CodeNotFound {
		t.Fatalf("跨集合版本被接受: %v", err)
	}
}
