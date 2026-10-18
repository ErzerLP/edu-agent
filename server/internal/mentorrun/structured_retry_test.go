package mentorrun

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/learningstart"
	"github.com/edu-agent/edu-agent/server/internal/research"
)

func TestPostgreSQLStartCitationRetryRevalidatesAndChargesBudget(t *testing.T) {
	for _, scenario := range []string{"repair", "still_invalid", "budget"} {
		t.Run(scenario, func(t *testing.T) {
			f := startFixture(t, "Go 并发")
			f.modelReply = func(w http.ResponseWriter, r *http.Request, n int) bool {
				var payload struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				if json.NewDecoder(r.Body).Decode(&payload) != nil {
					t.Error("invalid request")
					return true
				}
				var input struct {
					Sources []research.Source `json:"sources"`
					Support []struct {
						Citation research.Citation        `json:"citation"`
						Range    struct{ Start, End int } `json:"range"`
						Hash     string                   `json:"quote_sha256"`
					} `json:"citation_support"`
				}
				for _, m := range payload.Messages {
					if strings.HasPrefix(m.Content, "{") {
						_ = json.Unmarshal([]byte(m.Content), &input)
					}
				}
				if len(input.Support) == 0 || len(input.Sources) != 1 {
					t.Error("missing copyable support or unread source leaked")
					w.WriteHeader(500)
					return true
				}
				candidate := input.Support[0]
				fragment := input.Sources[0].Fragments[0]
				if candidate.Range.Start != fragment.Start || candidate.Range.End != fragment.Start+len(candidate.Citation.Quote) || candidate.Hash == "" {
					t.Error("candidate range/hash invalid")
				}
				if n == 2 && !strings.Contains(payload.Messages[len(payload.Messages)-1].Content, "citation_fragment_missing") {
					t.Error("retry lacks fixed rejection feedback")
				}
				citation := candidate.Citation
				if n == 1 || scenario == "still_invalid" {
					citation.FragmentID = "forged-private-fragment"
				}
				var value any = research.Synthesis{Points: []research.Point{{Text: "并发知识", Citations: []research.Citation{citation}}}, Gaps: []string{}, Examples: []string{}}
				if n == 3 {
					value = learningstart.Prepared{ConceptKey: "goroutines", Name: "并发", Prompt: "说明 goroutine 的并发执行", Criterion: "使用来源定义", Citations: []research.Citation{citation}}
				}
				content, _ := json.Marshal(value)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": string(content)}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30}})
				return true
			}
			if scenario == "budget" {
				f.create.RequestBudget = 4
			}
			r := f.accept(t)
			f.work(t)
			s := f.snapshot(t, r.RunID)
			switch scenario {
			case "repair":
				assertTaskUsage(t, f, r.RunID, "research", "checking_synthesis", 3)
				if s.Status != "succeeded" || s.StartLearning.Result == nil || f.calls.Load() != 3 || s.RequestsUsed != 6 || s.TokensLeft != 49910 {
					t.Fatalf("repair/budget incorrect: status=%s calls=%d requests=%d tokens=%d", s.Status, f.calls.Load(), s.RequestsUsed, s.TokensLeft)
				}
			case "still_invalid":
				assertTaskUsage(t, f, r.RunID, "research", "checking_synthesis", 2)
				if s.Reason != "invalid_source_citation" || s.StartLearning.Result != nil || s.Research.Synthesis != nil || f.calls.Load() != 2 {
					t.Fatalf("retry bypassed validation: status=%s reason=%s calls=%d", s.Status, s.Reason, f.calls.Load())
				}
			case "budget":
				assertTaskUsage(t, f, r.RunID, "research", "checking_synthesis", 1)
				if s.Status != "paused_budget" || f.calls.Load() != 1 || s.StartLearning.Result != nil {
					t.Fatalf("retry bypassed budget: status=%s calls=%d", s.Status, f.calls.Load())
				}
			}
		})
	}
}
