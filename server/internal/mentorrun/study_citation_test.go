package mentorrun

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacepg "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	"github.com/google/uuid"
)

func TestPostgreSQLStudyConversationOnlyAnswerWithSources(t *testing.T) {
	const answer = "根据我们之前的对话，你的专属示例编号是 47。"
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		var request struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		var material struct {
			Sources []knowledge.SourceDetail `json:"sources"`
		}
		var history bool
		for _, message := range request.Messages {
			if strings.HasPrefix(message.Content, "{") {
				_ = json.Unmarshal([]byte(message.Content), &material)
			}
			history = history || strings.Contains(message.Content, "我的专属示例编号是 47")
		}
		if len(material.Sources) == 0 || !history {
			t.Error("selected sources or the user's example are missing from context")
		}
		content := `{"answer":"已记住你的示例。资料定义可供后续学习。[1]","citations":[{"source":1,"fragment":1}],"exercise":null}`
		if n > 1 {
			raw, _ := json.Marshal(map[string]any{"answer": answer, "citations": []any{}, "exercise": nil})
			content = string(raw)
		}
		studioReply(w, content, "stop")
	})
	ctx := context.Background()
	direction := "根据资料学习，保留对话里的个人示例"
	if _, err := spacepg.New(f.pool, f.service.directions).Mutate(ctx, f.actor.Device.ID, learningspace.DefaultID, learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: 1, Name: "对话引用", Direction: &direction, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	source := selectedDocument(t, f, "资料只有通用定义，不含用户的个人示例。")
	id, err := f.service.NewConversation(ctx, f.actor, learningspace.DefaultID, NewConversation{ID: uuid.NewString(), Mode: "study"})
	if err != nil {
		t.Fatal(err)
	}
	for i, prompt := range []string{"记住：我的专属示例编号是 47。", "根据之前的对话，我的专属示例编号是多少？"} {
		c := historyPage(t, f, id, 0, 10).Conversation
		receipt, err := f.service.Submit(ctx, f.actor, learningspace.DefaultID, id, SubmitTurn{OperationID: uuid.NewString(), ExpectedVersion: c.Version, DirectionVersion: 1, Prompt: prompt, SourceSelection: &knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{source}}})
		if err != nil {
			t.Fatal(err)
		}
		f.work(t)
		snapshot := f.snapshot(t, receipt.RunID)
		if snapshot.Status != "succeeded" || snapshot.RequestsUsed != 1 {
			t.Fatalf("turn %d: status=%s reason=%s requests=%d", i+1, snapshot.Status, snapshot.Reason, snapshot.RequestsUsed)
		}
		if i == 1 && (snapshot.Output != answer || len(snapshot.Citations) != 0) {
			t.Fatal("conversation-only answer was changed or given a spurious source citation")
		}
	}
	page := historyPage(t, f, id, 0, 10)
	if len(page.Items) != 2 || page.Items[1].Output != answer || len(page.Items[1].Citations) != 0 || f.calls.Load() != 2 {
		t.Fatal("conversation-only answer did not reopen without retries or source citations")
	}
}

func TestPostgreSQLStudyCitationRejectsUnprovidedSlice(t *testing.T) {
	f := newExerciseFixture(t)
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		var material struct {
			Sources []knowledge.SourceDetail `json:"sources"`
		}
		for _, message := range request.Messages {
			if strings.HasPrefix(message.Content, "{") {
				_ = json.Unmarshal([]byte(message.Content), &material)
			}
		}
		if len(material.Sources) == 0 || len(material.Sources[0].Fragments) == 0 {
			t.Error("no source slices were provided")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fragments := material.Sources[0].Fragments
		if fragments[0].Proof == nil || fragments[0].Proof.Schema != 2 {
			t.Error("test did not exercise schema 2 source slices")
		}
		raw, _ := json.Marshal(map[string]any{"answer": "资料中的事实。[1]", "citations": []any{map[string]int{"source": 1, "fragment": len(fragments) + 1}}, "exercise": nil})
		studioReply(w, string(raw), "stop")
	}
	id, selection := studyExerciseConversation(t, f)
	c := historyPage(t, f.runtimeFixture, id, 0, 10).Conversation
	receipt, err := f.service.Submit(context.Background(), f.actor, learningspace.DefaultID, id, SubmitTurn{OperationID: uuid.NewString(), ExpectedVersion: c.Version, DirectionVersion: 1, Prompt: "解释资料中的定义", SourceSelection: &selection})
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	snapshot := f.snapshot(t, receipt.RunID)
	if snapshot.Status != "failed" || snapshot.Reason != "invalid_source_citation" || snapshot.Output != "" || len(snapshot.Citations) != 0 || f.calls.Load() != 2 {
		t.Fatalf("unprovided slice was accepted: status=%s reason=%s calls=%d", snapshot.Status, snapshot.Reason, f.calls.Load())
	}
}

func TestPostgreSQLStudyExerciseStillRequiresCitationsWithUncitedAnswer(t *testing.T) {
	for _, cited := range []bool{true, false} {
		name := "missing_exercise_citation"
		if cited {
			name = "cited_exercise"
		}
		t.Run(name, func(t *testing.T) {
			f := newExerciseFixture(t)
			var candidate map[string]any
			if err := json.Unmarshal([]byte(studyExerciseCandidate), &candidate); err != nil {
				t.Fatal(err)
			}
			candidate["answer"] = "根据我们之前的对话，接下来用一道题检验理解。"
			candidate["citations"] = []any{}
			if !cited {
				candidate["exercise"].(map[string]any)["citations"] = []any{}
			}
			raw, _ := json.Marshal(candidate)
			f.modelOverride = func(w http.ResponseWriter, r *http.Request) { studioReply(w, string(raw), "stop") }
			id, selection := studyExerciseConversation(t, f)
			receipt := studyExerciseTurn(t, f, id, selection)
			snapshot := f.snapshot(t, receipt.RunID)
			page := historyPage(t, f.runtimeFixture, id, 0, 10)
			if cited {
				if snapshot.Status != "succeeded" || len(snapshot.Citations) != 0 || page.Items[0].ExerciseID == "" || f.calls.Load() != 1 {
					t.Fatal("valid cited exercise with uncited answer was rejected")
				}
			} else if snapshot.Status != "failed" || page.Items[0].ExerciseID != "" || snapshot.Output != "" || f.calls.Load() != 2 {
				t.Fatal("exercise without source citations was published")
			}
		})
	}
}
