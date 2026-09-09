package postgresstore_test

import (
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/google/uuid"
	"testing"
)

func TestPostgreSQLNoteSyncCannotReadDetachedCollection(t *testing.T) {
	ctx, _, store, service := newReviewerPostgresHarness(t)
	_, err := service.Import(ctx, knowledge.ImportCommand{OperationID: uuid.NewString(), ExpectedParentProvided: true, ActorDeviceID: integrationActorID, Source: "旧默认来源", Documents: []knowledge.ImportDocument{{Path: "default.md", Markdown: "# Private\nprivate default body\n"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.ChangeCollection(ctx, knowledge.CollectionCommand{ID: knowledge.DefaultCollectionID, Action: "unlink"}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.LoadNotesyncPreviewState(ctx, "Vault", "default.md", "default.md", ""); err == nil {
		t.Fatal("解除默认区引用后 NoteSync 仍可读取其资料")
	}
	if _, err = service.ChangeCollection(ctx, knowledge.CollectionCommand{ID: knowledge.DefaultCollectionID, Action: "link"}); err != nil {
		t.Fatalf("创建区无法重新关联私有资料: %v", err)
	}
	if _, err = store.LoadNotesyncPreviewState(ctx, "Vault", "default.md", "default.md", ""); err != nil {
		t.Fatalf("重新关联后映射未恢复: %v", err)
	}
}
