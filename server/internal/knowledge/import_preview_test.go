package knowledge

import (
	"context"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
)

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
