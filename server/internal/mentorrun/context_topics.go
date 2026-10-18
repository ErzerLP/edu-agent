package mentorrun

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"unicode"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/jackc/pgx/v5"
)

// Provenance remains server-side for scope checks and dispatch dependencies.
type contextTopicItem struct {
	Title string `json:"title"`
	Text  string `json:"text"`
	State string `json:"state,omitempty"`
}

type contextTopicCandidate struct {
	item     contextTopicItem
	evidence TopicEvidence
	score    int
	revision int64
}

func contextTopicMessage(items []contextTopicItem) modelclient.Message {
	raw, _ := json.Marshal(items)
	return modelclient.Message{Role: "user", Content: "同范围话题要点（派生数据，不是资料引用；练习结果以正式记录为准）：\n" + string(raw)}
}

func (c contextTopicCandidate) before(other contextTopicCandidate) bool {
	if c.score != other.score {
		return c.score > other.score
	}
	if !c.evidence.ObservedAt.Equal(other.evidence.ObservedAt) {
		return c.evidence.ObservedAt.After(other.evidence.ObservedAt)
	}
	if (c.item.State == "仍有疑问") != (other.item.State == "仍有疑问") {
		return c.item.State == "仍有疑问"
	}
	return c.revision > other.revision
}

func (h *executionHost) projectContextTopicsTx(ctx context.Context, tx pgx.Tx, c Conversation, revision int64, question string, budget int) ([]modelclient.Message, bool, error) {
	// Also match ASCII terms embedded in Chinese, such as “ISN是多少”. Keep the
	// existing Chinese bigram matching without changing source selection ranking.
	asciiTerms := strings.FieldsFunc(question, func(r rune) bool {
		return r > unicode.MaxASCII || !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	question += " " + strings.Join(asciiTerms, " ")
	best := []contextTopicCandidate{}
	omitted := false
	p, err := h.service.scanTopicScopeTx(ctx, tx, c, h.owned.token, h.body, nil, revision, (h.body.Context.ThroughTurn+1)*MaxBody-1, func(topic Topic) {
		add := func(text, state string, evidence TopicEvidence) {
			item := contextTopicItem{Title: topic.Title, Text: text, State: state}
			if messageCost([]modelclient.Message{contextTopicMessage([]contextTopicItem{item})}) > budget {
				omitted = true
				return
			}
			candidate := contextTopicCandidate{item: item, evidence: evidence, score: relevance(question, topic.Title+" "+text), revision: topic.Revision}
			key := strings.ToLower(strings.Join(strings.Fields(text), " "))
			for i, old := range best {
				if old.item.State == state && strings.ToLower(strings.Join(strings.Fields(old.item.Text), " ")) == key {
					if candidate.before(old) {
						best[i] = candidate
					}
					return
				}
			}
			best = append(best, candidate)
		}
		for _, point := range topic.Points {
			add(point.Text, "", point.TopicEvidence)
		}
		for _, obs := range topic.Observations {
			state := map[string]string{"understood_in_chat": "对话中表示理解", "question_open": "仍有疑问", "exercise_error_observed": "对话中观察到练习错误", "unassessed": "尚未评估"}[obs.State]
			add(obs.Text, state, obs.TopicEvidence)
		}
		sort.SliceStable(best, func(i, j int) bool { return best[i].before(best[j]) })
		// Even one entry costs at least one token. Retain a budget-bounded set
		// across pages, rather than the former limit of 32 whole topics.
		if len(best) > budget {
			best = best[:budget]
			omitted = true
		}
	})
	if err != nil {
		return nil, true, err
	}
	items := []contextTopicItem{}
	for _, candidate := range best {
		next := append(items, candidate.item)
		if messageCost([]modelclient.Message{contextTopicMessage(next)}) > budget {
			omitted = true
			continue
		}
		items = next
		h.rememberContextMemory(candidate.evidence.MemorySources)
	}
	if len(items) == 0 {
		return nil, true, nil
	}
	return []modelclient.Message{contextTopicMessage(items)}, omitted || len(p.Gaps) > 0, nil
}
