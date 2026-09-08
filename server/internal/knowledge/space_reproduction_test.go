package knowledge

import (
	"context"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
)

func TestIssue3UnrelatedLearningSpaceCannotReadCatalog(t *testing.T) {
	s, _ := testKnowledgeService(t)
	imported, err := s.Import(context.Background(), ImportCommand{
		OperationID: uuid.NewString(), ExpectedParentProvided: true, Source: "默认区资料",
		Documents: []ImportDocument{{Path: "README.md", Markdown: "# Channel\nchannel private default material\n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := learningspace.WithScope(context.Background(), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	t.Run("检索", func(t *testing.T) {
		result, err := s.Retrieve(ctx, RetrievalCommand{Query: "channel"})
		if err == nil && len(result.Hits) != 0 {
			t.Fatalf("未关联的学习区读到了默认区资料：%d 条命中，revision=%s", len(result.Hits), result.KnowledgeRevisionID)
		}
	})
	t.Run("导出", func(t *testing.T) {
		result, err := s.Export(ctx, imported.Revision.ID)
		if err == nil && len(result.Documents) != 0 {
			t.Fatalf("未关联的学习区导出了默认区资料：%d 篇", len(result.Documents))
		}
	})
}
