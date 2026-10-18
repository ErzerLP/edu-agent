package mentorrun

import (
	"context"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/identity"
)

// Only references and projection versions enter a new run checkpoint.
// TurnStart is independent of the request's compressed message array.
type ConversationContext struct {
	MemorySources   []MemorySource `json:"memory_sources,omitempty"`
	InputHash       string         `json:"input_hash,omitempty"`
	InputProjection string         `json:"input_projection,omitempty"`
	ThroughTurn     int64          `json:"through_turn"`
	PreviousRun     string         `json:"previous_run,omitempty"`
	TurnStart       int            `json:"turn_start"`
	TopicRevision   int64          `json:"topic_revision,omitempty"`
}

func (h *executionHost) historyContext(ctx context.Context, budget int) ([][]modelclient.Message, bool, error) {
	if h.owned.ConversationID == "" || h.body.Context == nil {
		return nil, false, nil
	}
	tx, err := h.service.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(context.Background())
	g, err := gates(ctx, tx, false)
	if err != nil {
		return nil, false, err
	}
	if g != h.owned.Generation {
		return nil, false, ErrInactive
	}
	if err = actorGate(ctx, tx, h.owned.device, h.owned.token, true); err != nil {
		return nil, false, err
	}
	c, err := conversationRow(ctx, tx, identity.Credential{Device: identity.Device{ID: h.owned.device}}, h.owned.SpaceID, h.owned.ConversationID, g, false)
	if err != nil {
		return nil, false, err
	}
	if c.deleted {
		return nil, false, ErrNotFound
	}
	groups := [][]modelclient.Message{}
	used := 0
	scanned := 0
	skipped := false
	accept := func(body Body) (bool, error) {
		if !sameSourceScope(h.body.SourceManifest, body.SourceManifest) || !sameDirectionScope(h.body.Study, body.Study) {
			return true, nil
		}
		if body.SourceManifest == nil {
			for _, m := range body.Messages {
				for _, call := range m.ToolCalls {
					if call.Function.Name == "read_references" || call.Function.Name == "read_learning_context" {
						skipped = true
						return true, nil
					}
				}
			}
		}
		if len(body.MemorySources) > 0 {
			copy := *h
			copy.body = body
			if err := copy.validateMemorySources(ctx, tx); err != nil {
				skipped = true
				return true, nil
			}
		}
		messages := projectTools(committedMessages(body.Messages), 512)
		cost := messageCost(messages)
		if used+cost > budget {
			return false, nil
		}
		used += cost
		h.rememberContextMemory(body.MemorySources)
		groups = append(groups, messages)
		return true, nil
	}
	if !c.Saved {
		previous := h.body.Context.PreviousRun
		for previous != "" {
			item, e := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE id=$1 AND session_id=$2`, previous, c.ID))
			if e != nil {
				return nil, false, e
			}
			if !item.BodyAvailable || time.Now().After(item.ExpiresAt) || h.service.decode(&item) != nil {
				return groups, true, nil
			}
			scanned += max(1, messageCost(turnBody(item).Messages))
			ok, e := accept(turnBody(item))
			if e != nil {
				return nil, false, e
			}
			if !ok || scanned > max(budget, 256) {
				return groups, true, nil
			}
			previous = ""
			if item.body.Context != nil {
				previous = item.body.Context.PreviousRun
			}
		}
		return groups, previous != "" || skipped, nil
	}
	before := h.body.Context.ThroughTurn + 1
	// Bound both plaintext memory and scanning work, including incompatible scopes.
	for before > 1 {
		rows, e := tx.Query(ctx, `SELECT run_id::text,ordinal,ciphertext FROM learning_tutor_turns WHERE conversation_id=$1 AND ordinal<$2 ORDER BY ordinal DESC LIMIT 8`, c.ID, before)
		if e != nil {
			return nil, false, e
		}
		type turn struct {
			run     string
			ordinal int64
			raw     []byte
		}
		items := []turn{}
		for rows.Next() {
			var t turn
			if e = rows.Scan(&t.run, &t.ordinal, &t.raw); e != nil {
				rows.Close()
				return nil, false, e
			}
			items = append(items, t)
		}
		rows.Close()
		if e = rows.Err(); e != nil {
			return nil, false, e
		}
		if len(items) == 0 {
			return groups, skipped, nil
		}
		for _, t := range items {
			before = t.ordinal
			if t.raw == nil {
				continue
			}
			var body Body
			if e = openHistory(h.service.aead, historyAAD(c.ID, g, t.run), t.raw, &body); e != nil {
				return nil, false, e
			}
			if body.SourceManifest == nil && !body.TopicProvenance {
				var checkpoint []byte
				if e = tx.QueryRow(ctx, `SELECT checkpoint FROM learning_mentor_runs WHERE id=$1`, t.run).Scan(&checkpoint); e != nil {
					return nil, false, e
				}
				if checkpoint == nil {
					skipped = true
					continue
				}
				old, e := unseal(h.service.aead, t.run, checkpoint)
				if e != nil {
					return nil, false, e
				}
				body.MemorySources = old.MemorySources
			}
			scanned += max(1, messageCost(body.Messages))
			ok, e := accept(body)
			if e != nil {
				return nil, false, e
			}
			if !ok || scanned > max(budget, 256) {
				return groups, true, nil
			}
		}
	}
	return groups, before > 1 || skipped, nil
}
