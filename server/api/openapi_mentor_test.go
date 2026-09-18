package api_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
)

func TestMentorRunContractMatchesSnapshotsAndRecoveryRoutes(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	for name, value := range map[string]any{
		"TutorConversation": mentorrun.Conversation{ID: id, SpaceID: id, Generation: 1, Version: 1, GoalVersion: 1, Saved: true, Title: "历史", UpdatedAt: time.Now().UTC(), StorageState: "saved"},
		"MentorReceipt":     mentorrun.Receipt{OperationID: id, RunID: id, SessionID: id, Version: 1},
		"MentorEvent":       mentorrun.Event{RunID: id, SessionID: id, SpaceID: id, GoalID: id, Generation: 1, Version: 1, Seq: 1, Type: "accepted"},
		"MentorSnapshot":    mentorrun.Snapshot{Meta: mentorrun.Meta{RunID: id, SessionID: id, SpaceID: id, GoalID: id, GoalVersion: 1, Generation: 1, Version: 1, Watermark: 1, Status: "waiting_input", UpdatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}, Interaction: &mentorrun.Interaction{ID: id, Question: "请澄清", Choices: []string{}, CallID: "call"}},
	} {
		raw, _ := json.Marshal(value)
		var decoded any
		if json.Unmarshal(raw, &decoded) != nil {
			t.Fatal("合同夹具无效")
		}
		if err := doc.Components.Schemas[name].Value.VisitJSON(decoded, openapi3.EnableJSONSchema2020()); err != nil {
			t.Fatalf("%s 与服务端响应不一致：%v", name, err)
		}
	}
	for _, path := range []string{"/v1/learning/conversations", "/v1/learning/conversations/{conversationID}", "/v1/learning/conversations/{conversationID}/turns", "/v1/learning/runs", "/v1/learning/goals/{goalID}/runs", "/v1/learning/runs/{runID}", "/v1/learning/runs/{runID}/events", "/v1/learning/runs/{runID}/commands", "/v1/learning/operations/{operationID}"} {
		item := doc.Paths.Find(path)
		if item == nil {
			t.Fatalf("缺少正式运行接口：%s", path)
		}
		for _, operation := range item.Operations() {
			if operation.Security == nil || len(*operation.Security) == 0 || operation.Extensions["x-required-scope"] == nil {
				t.Fatalf("%s 缺少身份合同", path)
			}
		}
	}
}
