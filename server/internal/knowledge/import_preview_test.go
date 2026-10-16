package knowledge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/pdffixture"
	"github.com/google/uuid"
)

func TestShiftedPDFNodeReviewIncludesBoundedBeforeAfterEvidence(t *testing.T) {
	store := &previewGenerationStore{memoryCatalogStore: newMemoryCatalogStore(), generation: 1}
	s, err := NewService(store, NewCanonicalizer(), ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pages := []string{"原书25页 真值与机器数", "原书26页 模与补码", "原书27页 原码与补码", "原书28页 定点数", "原书29页 浮点数", "原书30页 移码 " + strings.Repeat("旧正文。", 60)}
	c := ImportCommand{OperationID: uuid.NewString(), ExpectedParentProvided: true, Source: "test", ActorDeviceID: uuid.NewString(), Documents: []ImportDocument{{Path: "教材.pdf.md", PDF: &PDFImport{Data: pdffixture.Build(pages...), AcceptPartial: true}}}}
	first, err := s.Import(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	c.OperationID, c.ExpectedParentRevisionID = uuid.NewString(), &first.Revision.ID
	shifted := append(append([]string{}, pages[1:]...), "原书31页 校验码 "+strings.Repeat("新正文。", 60))
	c.Documents = []ImportDocument{{Path: "教材.pdf.md", PDF: &PDFImport{Data: pdffixture.Build(shifted...), AcceptPartial: true}}}
	p, err := s.PreviewImport(t.Context(), c)
	if err != nil || p.Review == nil || len(p.Review.Documents) != 1 {
		t.Fatalf("document review: %+v %v", p, err)
	}
	c.OperationID = uuid.NewString()
	c.IdentityReviewBasisHash, c.IdentityReviewOperationID, c.IdentityReviewReceipt = p.Review.BasisHash, p.Review.OperationID, p.Review.Receipt
	c.DocumentResolutions = []DocumentResolution{{Locator: p.Review.Documents[0].Locator, Action: "preserve", DocumentID: first.Revision.Documents[0].Revision.DocumentID, Reason: "same book"}}
	p, err = s.PreviewImport(t.Context(), c)
	if err != nil || p.Review == nil || len(p.Review.Nodes) != 1 || p.Review.Nodes[0].Preorder != 5 {
		t.Fatalf("node review: %+v %v", p, err)
	}
	oldLast := first.Revision.Documents[0].Revision.Nodes[6]
	var found bool
	for _, candidate := range p.Review.Nodes[0].Candidates {
		if candidate.RevisionID != oldLast.ID {
			continue
		}
		found = true
		evidence := candidate.Evidence
		if evidence["title"] != "第 6 页" || evidence["incoming_title"] != "第 6 页" {
			t.Errorf("missing concrete page titles: %+v", evidence)
		}
		for key, prefix := range map[string]string{"excerpt": "原书30页 移码", "incoming_excerpt": "原书31页 校验码"} {
			value, _ := evidence[key].(string)
			if !strings.HasPrefix(value, prefix) || len([]rune(value)) > 161 || !strings.HasSuffix(value, "…") || strings.Contains(value, "解析状态") {
				t.Errorf("%s must show bounded actual page text, got %q", key, value)
			}
		}
	}
	if !found {
		t.Fatal("missing previous page-six candidate")
	}
}

type previewGenerationStore struct {
	*memoryCatalogStore
	generation int64
}

func (s *previewGenerationStore) ImportGeneration(context.Context) (int64, error) {
	return s.generation, nil
}

type racingPreviewStore struct {
	*previewGenerationStore
	beforeHead func()
}

func (s *racingPreviewStore) Head(ctx context.Context) (*KnowledgeRevision, error) {
	if s.beforeHead != nil {
		hook := s.beforeHead
		s.beforeHead = nil
		hook()
	}
	return s.memoryCatalogStore.Head(ctx)
}

func TestImportConfirmationReturnsConcurrentCompletedOperation(t *testing.T) {
	store := &racingPreviewStore{previewGenerationStore: &previewGenerationStore{memoryCatalogStore: newMemoryCatalogStore(), generation: 1}}
	s, err := NewService(store, NewCanonicalizer(), ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c := ImportCommand{OperationID: uuid.NewString(), ExpectedParentProvided: true, ActorDeviceID: uuid.NewString(), Source: "测试", Documents: []ImportDocument{{Path: "a.md", Markdown: "# A\n"}}}
	p, err := s.PreviewImport(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	prepared, base, err := s.planImport(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	prepared.RequestHash = importConfirmationHash(t.Context(), c)
	summary := importSummary(t.Context(), c, *prepared, base)
	// 精确模拟首次查回执之后、读取 head 之前，另一请求提交相同操作。
	store.beforeHead = func() {
		if _, err := store.memoryCatalogStore.CommitImport(t.Context(), *prepared); err != nil {
			t.Fatal(err)
		}
		store.mu.Lock()
		record := store.operations[c.OperationID]
		record.Result.Summary = &summary
		store.operations[c.OperationID] = record
		store.mu.Unlock()
	}
	result, err := s.ConfirmImport(t.Context(), ConfirmImportCommand{Request: c, Receipt: p.Receipt})
	if err != nil || !result.Replayed || result.Revision.ID != prepared.Revision.ID {
		t.Fatalf("迟到父版本冲突覆盖了成功回执：%+v %v", result, err)
	}
}

func TestImportPreviewReceiptBindsInputTargetGenerationAndExpiry(t *testing.T) {
	store := &previewGenerationStore{memoryCatalogStore: newMemoryCatalogStore(), generation: 1}
	now := time.Now()
	s, err := NewService(store, NewCanonicalizer(), ServiceOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	c := ImportCommand{OperationID: uuid.NewString(), ExpectedParentProvided: true, Source: "测试", ActorDeviceID: uuid.NewString(), Documents: []ImportDocument{{Path: "a.md", Markdown: "# A\n原始内容"}}}
	p, err := s.PreviewImport(t.Context(), c)
	if err != nil || p.Receipt == "" {
		t.Fatalf("预览：%+v %v", p, err)
	}
	for _, mutate := range []func(*ImportCommand){func(c *ImportCommand) { c.Source = "different" }, func(c *ImportCommand) { c.ActorDeviceID = uuid.NewString() }, func(c *ImportCommand) {
		c.Documents = []ImportDocument{{Path: "other.md", Markdown: "# A\n原始内容"}}
	}, func(c *ImportCommand) {
		c.DocumentResolutions = []DocumentResolution{{Locator: "x", Action: "new", Reason: "new"}}
	}} {
		changed := c
		mutate(&changed)
		if _, err := s.ConfirmImport(t.Context(), ConfirmImportCommand{Request: changed, Receipt: p.Receipt}); ErrorCode(err) != CodeImportPreviewStale {
			t.Fatalf("变更复用确认：%v", err)
		}
	}
	spaceCtx, _ := learningspace.WithScope(t.Context(), uuid.NewString())
	collectionCtx, _ := WithCollection(t.Context(), uuid.NewString())
	for _, ctx := range []context.Context{spaceCtx, collectionCtx} {
		if _, err := s.ConfirmImport(ctx, ConfirmImportCommand{Request: c, Receipt: p.Receipt}); ErrorCode(err) != CodeImportPreviewStale {
			t.Fatalf("跨目标确认：%v", err)
		}
	}
	store.generation = 2
	if _, err := s.ConfirmImport(t.Context(), ConfirmImportCommand{Request: c, Receipt: p.Receipt}); ErrorCode(err) != CodeImportPreviewStale {
		t.Fatalf("隐私清除后确认：%v", err)
	}
	store.generation = 1
	now = now.Add(16 * time.Minute)
	if _, err := s.ConfirmImport(t.Context(), ConfirmImportCommand{Request: c, Receipt: p.Receipt}); ErrorCode(err) != CodeImportPreviewStale {
		t.Fatalf("超时确认：%v", err)
	}
	if len(store.operations) != 0 || len(store.revisions) != 0 {
		t.Fatal("无效确认写入 canonical")
	}
}
