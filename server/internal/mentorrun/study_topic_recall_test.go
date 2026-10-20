package mentorrun

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/packages/agentcore"
	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacepg "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/google/uuid"
)

// Exercise Submit -> generateStudy -> assembleContext -> the model HTTP request.
// Only the remote model is stubbed; history, indexing and scope gates are real.
func TestPostgreSQLStudyRecallCommittedTopicsDuringIndexing(t *testing.T) {
	const seed = `记住我的专属例子"海燕-47"：客户端 ISN 是 31415，服务端 ISN 是 27182，后面我会考你。`
	const firstAnswer = "已记住海燕-47，第三次握手 seq=31416、ack=27183。"
	const fact = "海燕-47：客户端 ISN=31415，服务端 ISN=27182；第三次握手 seq=31416、ack=27183。"
	const question = "海燕-47 的客户端和服务端 ISN 分别是多少？第三次握手的 seq 和 ack 是多少？"
	for _, status := range []string{"ready", "queued", "running"} {
		t.Run(status, func(t *testing.T) {
			var last struct {
				Messages  []modelclient.Message `json:"messages"`
				MaxTokens int                   `json:"max_tokens"`
			}
			f := fixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				if err := json.NewDecoder(r.Body).Decode(&last); err != nil {
					t.Error(err)
					return
				}
				var batch TopicBatch
				if len(last.Messages) > 1 && json.Unmarshal([]byte(last.Messages[1].Content), &batch) == nil && len(batch.Messages) > 0 {
					anchors := []string{}
					for _, message := range batch.Messages {
						if message.Content == seed || message.Content == firstAnswer {
							anchors = append(anchors, message.ID)
						}
					}
					if len(anchors) != 2 {
						t.Error("indexing did not receive both first-turn messages")
					}
					raw, _ := json.Marshal(map[string]any{"topics": []any{map[string]any{
						"id": "", "title": "TCP 三次握手（海燕-47）", "anchor_ids": anchors,
						"points": []any{map[string]any{"text": fact, "anchor_ids": anchors}}, "observations": []any{},
					}}})
					studioReply(w, string(raw), "stop")
					return
				}
				answer := strings.Repeat("发送窗口限制在途字节，接收窗口反映接收缓冲区的容量。", 50)
				switch last.Messages[len(last.Messages)-1].Content {
				case seed:
					answer = firstAnswer
				case question:
					answer = "根据我们之前的对话，" + fact
				}
				raw, _ := json.Marshal(map[string]any{"answer": answer, "citations": []any{}, "exercise": nil})
				studioReply(w, string(raw), "stop")
			})
			ctx := context.Background()
			limits := f.settings.View().Limits
			limits.OutputTokens = 8192
			updateLimits := func() {
				t.Helper()
				if _, err := f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, Target: settings.Mentor, Limits: &limits}); err != nil {
					t.Fatal(err)
				}
			}
			updateLimits()
			direction := "学习 TCP 与子网划分，并保留对话中的专属示例"
			if _, err := spacepg.New(f.pool, f.service.directions).Mutate(ctx, f.actor.Device.ID, learningspace.DefaultID, learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: 1, Name: "压缩召回", Direction: &direction, Status: "active"}); err != nil {
				t.Fatal(err)
			}
			source := selectedDocument(t, f, "TCP 向应用提供有序字节流，滑动窗口控制在途字节数。资料不包含对话中的专属示例。")
			id, err := f.service.NewConversation(ctx, f.actor, learningspace.DefaultID, NewConversation{ID: uuid.NewString(), Mode: "study"})
			if err != nil {
				t.Fatal(err)
			}
			send := func(prompt string) Receipt {
				t.Helper()
				c := historyPage(t, f, id, 0, 10).Conversation
				receipt, err := f.service.Submit(ctx, f.actor, learningspace.DefaultID, id, SubmitTurn{OperationID: uuid.NewString(), ExpectedVersion: c.Version, DirectionVersion: 1, Prompt: prompt, SourceSelection: &knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{source}}})
				if err != nil {
					t.Fatal(err)
				}
				f.work(t)
				if s := f.snapshot(t, receipt.RunID); s.Status != "succeeded" || s.RequestsUsed != 1 {
					t.Fatalf("study_answer: status=%s reason=%s requests=%d", s.Status, s.Reason, s.RequestsUsed)
				}
				return receipt
			}
			for _, prompt := range []string{seed, "说明等长子网划分。", "说明 VLSM 分配。", "说明滑动窗口。"} {
				send(prompt)
			}
			// The fourth completed turn queues real automatic topic indexing.
			if p := readTopics(t, f, id); p.Status != "queued" {
				t.Fatalf("automatic indexing was not queued: %s", p.Status)
			}
			f.work(t)
			if p := readTopics(t, f, id); p.Status != "ready" || p.Revision != 1 || p.CoveredMessages != 8 || len(p.Items) != 1 || p.Items[0].Points[0].Text != fact {
				t.Fatalf("first-turn fact was not committed with complete coverage: %+v", p)
			}
			limits.ContextTokens = 14192
			updateLimits()
			// Freeze the head in each incremental-worker state without racing a
			// goroutine. The committed revision and its evidence remain unchanged.
			if _, err := f.pool.Exec(ctx, `UPDATE learning_topic_index_heads SET status=$2 WHERE conversation_id=$1`, id, status); err != nil {
				t.Fatal(err)
			}
			receipt := send(question)
			var topic, material bool
			for _, message := range last.Messages {
				if strings.Contains(message.Content, seed) || strings.Contains(message.Content, firstAnswer) {
					t.Error("first-turn raw messages still fit; test did not require topic recall")
				}
				if strings.HasPrefix(message.Content, "同范围话题要点") && strings.Contains(message.Content, fact) {
					topic = true
				}
				var sources struct {
					Sources []knowledge.SourceDetail `json:"sources"`
					Support []json.RawMessage        `json:"citation_support"`
				}
				if json.Unmarshal([]byte(message.Content), &sources) == nil && len(sources.Sources) > 0 && len(sources.Support) > 0 {
					material = true
				}
			}
			if !topic {
				t.Errorf("study_answer request omitted the committed first-turn fact while topic index was %s", status)
			}
			for _, instruction := range []string{"话题要点是先前对话内容的可靠摘要", "用户询问对话中说过的内容时，应直接根据相关要点回答", "根据我们之前的对话", "无需资料引用", "资料事实仍须引用"} {
				if !strings.Contains(last.Messages[0].Content, instruction) {
					t.Errorf("study_answer request lacks conversation-recall instruction %q", instruction)
				}
			}
			for _, message := range last.Messages {
				if strings.HasPrefix(message.Content, "同范围话题要点") && !strings.Contains(message.Content, "可靠对话摘要") {
					t.Error("topic message does not identify its points as a reliable conversation summary")
				}
			}
			if !material || last.MaxTokens != 8192 || last.Messages[len(last.Messages)-1].Content != question {
				t.Fatal("request lost selected sources, citation support, output allowance or the original question")
			}
			estimate := agentcore.NewTokenEstimator().EstimateRequest(modelclient.Request{Messages: last.Messages})
			if estimate+8192+710+len(studyExerciseSchema())+512 > 14192 {
				t.Fatal("study_answer request exceeded the configured context window")
			}
			if s := f.snapshot(t, receipt.RunID); len(s.Citations) != 0 || !strings.Contains(s.Output, historyNotice) {
				t.Fatal("compressed conversation-only answer lost its notice or acquired a source citation")
			}
			t.Logf("status=%s first_turn_raw=false topic_fact=%t input_estimate=%d", status, topic, estimate)
		})
	}
}
