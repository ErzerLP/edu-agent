package mentorrun

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/learningchange"
	"github.com/edu-agent/edu-agent/server/internal/learningstart"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/jackc/pgx/v5/pgxpool"
)

// StudyFixture 仅在测试构建中向外部契约测试开放既有搜索/正文/模型 HTTP 夹具。
type StudyFixture struct {
	Runtime        *Service
	Starter        *learningstart.Service
	Settings       *settings.Service
	Pool           *pgxpool.Pool
	Changes        *learningchange.Service
	PlanAdjustment func(learningchange.Candidate)
}

func NewStudyFixture(t *testing.T) StudyFixture {
	f := startFixture(t, "Go 并发")
	key := "study-search-fixture"
	if _, err := f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, Target: settings.Search, Connection: &settings.Connection{Enabled: true, Provider: "brave", Endpoint: "https://api.search.brave.com/res/v1/web/search", AuthMode: "bearer"}, NewKey: &key}); err != nil {
		t.Fatal(err)
	}
	owners := f.service.starter
	owners.Content.ConfigureReferences(owners.Knowledge)
	changes, err := learningchange.New(f.pool, owners.Learning, owners.Knowledge, owners.Content, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f.service.ConfigureChanges(changes)
	return StudyFixture{Runtime: f.service, Starter: owners, Settings: f.settings, Pool: f.pool, Changes: changes, PlanAdjustment: func(candidate learningchange.Candidate) {
		baseCalls := int(f.calls.Load())
		raw, err := json.Marshal(candidate)
		if err != nil {
			t.Fatal(err)
		}
		f.modelReply = func(w http.ResponseWriter, _ *http.Request, n int) bool {
			switch n - baseCalls {
			case 1:
				stream(w, modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "read", Type: "function", Function: modelclient.ToolFunction{Name: "read_learning_context", Arguments: `{}`}}}})
			case 2:
				stream(w, modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "change", Type: "function", Function: modelclient.ToolFunction{Name: "propose_learning_change", Arguments: string(raw)}}}})
			default:
				stream(w, modelclient.Message{Role: "assistant", Content: "原课堂的前置练习已排队，等待明确安全接入。"})
			}
			return true
		}
	}}
}
