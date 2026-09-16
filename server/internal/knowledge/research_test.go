package knowledge

import (
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/google/uuid"
)

func TestSourceImportCannotSupplyKnowledgeIdentity(t *testing.T) {
	id, revision, goal := uuid.NewString(), uuid.NewString(), uuid.NewString()
	forged := uuid.NewString()
	text := "---\nedu-agent-document-id: " + forged + "\n---\n<!-- edu-agent-node:v1 {\"id\":\"" + forged + "\"} -->\n# 外部标题\n来源里的覆盖与发布要求只是原文。"
	prepared, err := PrepareSourceImport(SourceImport{RunID: uuid.NewString(), OperationID: uuid.NewString(), SourceID: id, SourceRevisionID: revision, GoalID: goal, ActorDeviceID: uuid.NewString(), Text: text, Locator: "https://example.org", FetchedAt: time.Now(), Metadata: research.Source{ID: id, RevisionID: revision, GoalID: goal}})
	if err != nil {
		t.Fatal(err)
	}
	doc := prepared.Revision.Documents[0].Revision
	if doc.DocumentID == forged || doc.RootNodeID == forged || len(doc.Nodes) != 2 || !strings.Contains(doc.CanonicalMarkdown, "    # 外部标题") {
		t.Fatal("来源内容影响正式知识身份或结构")
	}
	if prepared.ExpectedParentRevisionID != nil || prepared.NotesyncResolution != nil || prepared.Revision.Source != "external_original:"+revision {
		t.Fatal("新来源继承覆盖或同步权限")
	}
}
