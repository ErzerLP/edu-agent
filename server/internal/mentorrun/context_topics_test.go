package mentorrun

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore"
	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/settings"
)

// Keep the real scope/evidence gates, but replace indexed content and make the
// old raw turn too large to fit so recall must come from topic points.
func contextTopicFixture(t *testing.T, edit func(*topicBody)) *executionHost {
	t.Helper()
	f := topicFixture(t)
	id := newHistory(t, f, true, "")
	receipt := sendHistory(t, f, id, "早期讨论")
	f.work(t)
	requestTopics(t, f, id)
	f.work(t)
	page := readTopics(t, f, id)
	ctx := context.Background()
	for _, item := range page.Items {
		var raw []byte
		if err := f.pool.QueryRow(ctx, `SELECT ciphertext FROM learning_conversation_topics WHERE id=$1`, item.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var body topicBody
		if err := openHistory(f.service.aead, historyAAD(id, 1, "topic:"+item.ID), raw, &body); err != nil {
			t.Fatal(err)
		}
		edit(&body)
		raw, err := sealHistory(f.service.aead, historyAAD(id, 1, "topic:"+item.ID), body)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.pool.Exec(ctx, `UPDATE learning_conversation_topics SET ciphertext=$2 WHERE id=$1`, item.ID, raw); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := sealHistory(f.service.aead, historyAAD(id, 1, receipt.RunID), Body{TopicProvenance: true, Messages: []modelclient.Message{{Role: "user", Content: strings.Repeat("很长的旧原文", 5000)}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(ctx, `UPDATE learning_tutor_turns SET ciphertext=$2 WHERE run_id=$1`, receipt.RunID, raw); err != nil {
		t.Fatal(err)
	}
	return &executionHost{service: f.service, owned: row{Meta: Meta{ConversationID: id, SpaceID: learningspace.DefaultID, Generation: 1, Saved: true}, device: f.actor.Device.ID, token: f.actor.TokenID}, body: Body{Context: &ConversationContext{ThroughTurn: 1}}, limits: settings.Limits{ContextTokens: 14192, OutputTokens: 8192}}
}

func TestPostgreSQLContextTopicsRecallPointFromOversizedTopic(t *testing.T) {
	const fact = "海燕-47：客户端 ISN=31415，服务端 ISN=27182；第三次握手 seq=31416、ack=27183。"
	h := contextTopicFixture(t, func(body *topicBody) {
		point := body.Points[0]
		body.Title = "TCP 三次握手示例"
		body.Points = nil
		body.Observations = nil
		for i := 0; i < 11; i++ {
			point.Text = strconv.Itoa(i) + strings.Repeat("其他握手背景。", 70)
			body.Points = append(body.Points, point)
		}
		point.Text = fact
		body.Points = append(body.Points, point)
	})
	h.body.Messages = []modelclient.Message{{Role: "user", Content: "海燕示例的 ISN 和第三次握手数值是多少？"}}
	plan, err := h.assembleContext(context.Background(), modelclient.Request{MaxTokens: 8192, Messages: []modelclient.Message{{Role: "system", Content: "根据已提供的要点回答"}}}, nil, h.body.Messages)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(plan.Request)
	if !strings.Contains(string(raw), fact) {
		t.Fatalf("超出话题池的早期话题丢失了相关要点：%s", raw)
	}
	if strings.Count(string(raw), fact) != 1 {
		t.Fatal("跨话题的重复要点挤占预算")
	}
	if got := agentcore.NewTokenEstimator().EstimateRequest(plan.Request) + plan.ReservedOutput + 710; got > 14192 {
		t.Fatalf("请求超出窗口：%d", got)
	}
	for _, field := range []string{"anchor_ids", "scope_fingerprint", "observed_at", "privacy_generation", "evidence", "anchor_count", "exercise_related", `"id":`, `"revision":`} {
		if strings.Contains(string(raw), field) {
			t.Errorf("模型投影包含元数据 %q", field)
		}
	}
	// Even a pool much smaller than the reproduction allowance can hold facts;
	// the envelope and explanatory prefix must be charged to that same pool.
	for _, budget := range []int{80, 128, 240, 958} {
		messages, _, err := h.contextTopics(context.Background(), budget)
		if err != nil || messageCost(messages) > budget {
			t.Fatalf("话题池 %d 超限或失败：%v, %v", budget, messages, err)
		}
		if budget >= 128 && (len(messages) == 0 || !strings.Contains(messages[0].Content, fact)) {
			t.Errorf("小话题池 %d 未装入可容纳的相关要点", budget)
		}
	}
}

func TestPostgreSQLContextTopicsSelectRelevantPoints(t *testing.T) {
	h := contextTopicFixture(t, func(body *topicBody) {
		point := body.Points[0]
		body.Observations = nil
		if body.Title == "TCP" {
			point.ObservedAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
			point.Text = "ISN=31415；海燕-47。"
			body.Points = []TopicPoint{point}
		} else {
			point.ObservedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			point.Text = "最新子网划分说明。"
			body.Points = []TopicPoint{point}
		}
	})
	for _, question := range []string{"ISN是多少？", "海燕的数值？"} {
		h.body.Messages = []modelclient.Message{{Role: "user", Content: question}}
		// Each point fits, but the two together do not. Ranking must choose the
		// older relevant point over a recent unrelated topic.
		messages, _, err := h.contextTopics(context.Background(), 64)
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) != 1 || !strings.Contains(messages[0].Content, "31415") {
			t.Fatalf("未按当前问题 %q 选择较早的相关要点：%v", question, messages)
		}
		if strings.Contains(messages[0].Content, "最新子网划分说明") || messageCost(messages) > 64 {
			t.Fatal("相关要点投影超限")
		}
	}
}

func TestPostgreSQLContextTopicsPrioritizeRecentQuestions(t *testing.T) {
	h := contextTopicFixture(t, func(body *topicBody) {
		evidence := body.Points[0].TopicEvidence
		body.Points = nil
		body.Observations = nil
		if body.Title == "TCP" {
			evidence.ObservedAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
			body.Observations = []TopicObservation{{State: "question_open", Text: "早期疑问", Evidence: "user_report", TopicEvidence: evidence}}
		} else {
			evidence.ObservedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			body.Points = []TopicPoint{{Text: "最近要点", TopicEvidence: evidence}}
			body.Observations = []TopicObservation{{State: "unassessed", Text: "最近未评估", Evidence: "user_report", TopicEvidence: evidence}, {State: "question_open", Text: "最近疑问", Evidence: "user_report", TopicEvidence: evidence}}
		}
	})
	h.body.Messages = []modelclient.Message{{Role: "user", Content: "继续"}}
	messages, _, err := h.contextTopics(context.Background(), 64)
	if err != nil || len(messages) != 1 {
		t.Fatal(messages, err)
	}
	if !strings.Contains(messages[0].Content, "最近疑问") || !strings.Contains(messages[0].Content, "仍有疑问") || strings.Contains(messages[0].Content, "早期疑问") || strings.Contains(messages[0].Content, "最近要点") || strings.Contains(messages[0].Content, "最近未评估") {
		t.Fatal("同相关度没有优先最近的疑问观察", messages)
	}
	if messageCost(messages) > 64 {
		t.Fatal("观察投影超出预算")
	}
}

func TestPostgreSQLContextTopicsUseUnfilledHistoryRoom(t *testing.T) {
	const fact = "本轮专属例子命名为海燕-47，客户端 ISN=31415，服务端 ISN=27182。"
	const question = "回忆最初第一轮的 TCP 三次握手示例，客户端和服务端 ISN 各是多少？"
	h := contextTopicFixture(t, func(body *topicBody) {
		point := body.Points[0]
		body.Points = nil
		body.Observations = nil
		if body.Title == "TCP" {
			point.Text = fact
			body.Points = []TopicPoint{point}
		} else {
			body.Title = "最初第一轮 TCP 三次握手示例与 ISN 回忆"
			for i := 0; i < 8; i++ {
				point.Text = strconv.Itoa(i) + "：用户要求" + question + strings.Repeat("本次上下文中未提供首轮摘要，不能猜测。", 4)
				body.Points = append(body.Points, point)
			}
		}
	})
	// Fixed schemas leave a small initial topic allowance. The huge raw turn
	// cannot fill the history pool, so that unused room can retain more facts.
	h.schemaTokens = 2400
	h.body.Messages = []modelclient.Message{{Role: "user", Content: question}}
	plan, err := h.assembleContext(context.Background(), modelclient.Request{MaxTokens: 8192, Messages: []modelclient.Message{{Role: "system", Content: "系统"}}}, nil, h.body.Messages)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(plan.Request)
	if !strings.Contains(string(raw), fact) {
		t.Fatal("原文池空余没有转移给话题，早期数值被重复提问挤掉", string(raw))
	}
	if got := agentcore.NewTokenEstimator().EstimateRequest(plan.Request) + 8192 + 710 + 2400; got > 14192 {
		t.Fatalf("转移额度后超窗：%d", got)
	}
}
