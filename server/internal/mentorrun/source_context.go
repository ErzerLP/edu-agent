package mentorrun

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/edu-agent/edu-agent/packages/agentcore"
	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/jackc/pgx/v5"
)

func relevance(question, text string) int {
	question = strings.ToLower(question)
	text = strings.ToLower(text)
	score := 0
	for _, word := range strings.Fields(question) {
		if strings.Contains(text, word) {
			score += 10
		}
	}
	// Overlapping CJK bigrams also work without a language-specific tokenizer.
	runes := []rune(question)
	for i := 1; i < len(runes); i++ {
		if runes[i] > 127 && strings.Contains(text, string(runes[i-1:i+1])) {
			score++
		}
	}
	return score
}
func (h *executionHost) selectSourceSlices(ctx context.Context, tx pgx.Tx) ([]knowledge.SourceDetail, error) {
	if h.body.SourceManifest == nil {
		return nil, nil
	}
	if len(h.body.SourceManifest.Entries) > 0 {
		if err := sourceActor(ctx, tx, h.owned.token); err != nil {
			return nil, err
		}
	}
	scoped, err := learningspace.WithScope(ctx, h.owned.SpaceID)
	if err != nil {
		return nil, err
	}
	store := knowledgepg.New(h.service.pool)
	question := ""
	for _, m := range h.body.Messages {
		if m.Role == "user" {
			question = m.Content
		}
	}
	type candidate struct {
		node  knowledge.SourceDirectoryEntry
		score int
	}
	best := []candidate{}
	cursor := ""
	for {
		page, e := store.ReadSourceDirectoryTx(scoped, tx, *h.body.SourceManifest, cursor, 64)
		if e != nil {
			return nil, e
		}
		for _, node := range page.Items {
			best = append(best, candidate{node, relevance(question, node.Title)})
		}
		sort.SliceStable(best, func(i, j int) bool { return best[i].score > best[j].score })
		if len(best) > 16 {
			best = best[:16]
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	// Account for identity/proof JSON as well as text. Study generation additionally
	// sends citation support, hence leaves half this pool for that representation.
	budget := max(0, (h.limits.ContextTokens-h.limits.OutputTokens-contextMargin(h.limits.ContextTokens)-3000)*35/100)
	if h.sourceLimit != nil {
		budget = *h.sourceLimit
	}
	if h.body.Study != nil {
		budget /= 2
	}
	result := []knowledge.SourceDetail{}
	for _, c := range best {
		n := c.node
		end := min(n.Range.End, n.Range.Start+4096)
		for end > n.Range.Start {
			detail, e := store.ReadSourceSliceTx(scoped, tx, *h.body.SourceManifest, knowledge.SourceSliceRequest{SourceIdentity: n.SourceIdentity, NodeID: n.NodeID, NodeRevisionID: n.NodeRevisionID, Start: n.Range.Start, End: end})
			if e != nil {
				return nil, e
			}
			next := append([]knowledge.SourceDetail(nil), result...)
			found := false
			for i := range next {
				if next[i].Identity() == detail.Identity() {
					next[i].Fragments = append(append([]knowledge.SourceFragment(nil), next[i].Fragments...), detail.Fragments...)
					found = true
					break
				}
			}
			if !found {
				next = append(next, detail)
			}
			raw, _ := json.Marshal(next)
			if messageCost([]modelclient.Message{{Role: "user", Content: string(raw)}}) <= budget {
				result = next
				break
			}
			end = n.Range.Start + (end-n.Range.Start)/2
			if end-n.Range.Start < 32 {
				break
			}
		}
	}
	h.sources = result
	return result, nil
}

// The current question is reserved before selecting source slices. Definitions,
// direction and schema are fixed; an empty source pool returns room to history.
func (h *executionHost) sourceWindow(fixed modelclient.Request) {
	budget := h.limits.ContextTokens - h.limits.OutputTokens - contextMargin(h.limits.ContextTokens) - agentcore.NewTokenEstimator().EstimateRequest(fixed) - h.schemaTokens - 256
	current := 0
	for _, m := range h.body.Messages {
		if m.Role == "user" {
			current = messageCost([]modelclient.Message{m})
		}
	}
	limit := max(0, min(budget*35/100, budget-current-256))
	h.sourceLimit = &limit
	h.sources = nil
}
