package mentorrun

import (
	"context"
	"slices"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/identity"
)

func (s *Service) Command(ctx context.Context, actor identity.Credential, space, id string, c Command) (Receipt, error) {
	if !validID(id) || !validID(space) || !validID(c.OperationID) || c.ExpectedVersion < 1 {
		return Receipt{}, ErrInvalid
	}
	if c.Kind != "stop" && c.Kind != "clear" && c.Kind != "respond" && c.Kind != "continue_budget" && c.Kind != "retry_start" {
		return Receipt{}, ErrInvalid
	}
	if c.Kind != "respond" && (c.InteractionID != "" || c.Answer != "") || c.Kind != "continue_budget" && c.Kind != "retry_start" && (c.RequestBudget != 0 || c.TokenBudget != 0) {
		return Receipt{}, ErrInvalid
	}
	hash := requestHash("command", space, id, c)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Receipt{}, err
	}
	defer tx.Rollback(context.Background())
	generation, err := gates(ctx, tx, true)
	if err != nil {
		return Receipt{}, err
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, true); err != nil {
		return Receipt{}, err
	}
	if r, found, e := operation(ctx, tx, actor.Device.ID, c.OperationID, hash); e != nil || found {
		return r, e
	}
	// 生命周期检查先于运行行锁，停止和清除仍可在目标暂停后执行。
	if c.Kind == "respond" || c.Kind == "continue_budget" || c.Kind == "retry_start" {
		var goal string
		var version int64
		if err = tx.QueryRow(ctx, `SELECT goal_id::text,goal_version FROM learning_mentor_runs WHERE id=$1 AND device_id=$2 AND space_id=$3`, id, actor.Device.ID, space).Scan(&goal, &version); err != nil {
			return Receipt{}, ErrNotFound
		}
		if _, err = goalGate(ctx, tx, space, goal, version); err != nil {
			return Receipt{}, err
		}
		if err = s.quota(ctx, tx, 1024); err != nil {
			return Receipt{}, err
		}
	}
	item, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE id=$1 AND device_id=$2 AND space_id=$3 AND privacy_generation=$4 FOR UPDATE`, id, actor.Device.ID, space, generation))
	if err != nil {
		return Receipt{}, err
	}
	if item.Version != c.ExpectedVersion {
		return Receipt{}, ErrConflict
	}
	if (c.Kind == "respond" || c.Kind == "continue_budget") && (time.Now().After(item.ExpiresAt) || !item.BodyAvailable) {
		return Receipt{}, ErrInactive
	}
	if c.Kind != "clear" {
		if err = s.decode(&item); err != nil {
			return Receipt{}, err
		}
	}
	switch c.Kind {
	case "clear":
		if !item.BodyAvailable {
			return Receipt{}, ErrConflict
		}
		item.body = Body{}
		item.BodyAvailable = false
		item.Status = "cancelled"
		item.Reason = "cleared"
		item.lease = nil
		item.leaseUntil = nil
		if _, err = tx.Exec(ctx, `UPDATE learning_tutor_turns SET ciphertext=NULL WHERE run_id=$1`, id); err != nil {
			return Receipt{}, err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM learning_mentor_events WHERE run_id=$1`, id); err != nil {
			return Receipt{}, err
		}
	case "stop":
		if terminal(item.Status) || item.Status == "cancelling" {
			return Receipt{}, ErrConflict
		}
		if item.Status == "running" || item.Status == "cancelling" {
			item.Status = "cancelling"
		} else {
			item.Status = "cancelled"
		}
		item.Reason = "user_stopped"
	case "respond":
		pending := item.body.Interaction
		if pending == nil || (item.Status != "waiting_input" && item.Status != "waiting_approval") || pending.ID != c.InteractionID {
			return Receipt{}, ErrConflict
		}
		if !validText(c.Answer, 8000) || (len(pending.Choices) > 0 && !slices.Contains(pending.Choices, c.Answer)) {
			return Receipt{}, ErrInvalid
		}
		answer := c.Answer
		if pending.MemoryCandidateID != "" {
			answer, err = s.memoryResponse(ctx, tx, item, pending, c.Answer, c.OperationID)
			if err != nil {
				return Receipt{}, err
			}
		}
		item.body.Messages = append(item.body.Messages, modelclient.Message{Role: "tool", ToolCallID: pending.CallID, Content: answer})
		item.body.Interaction = nil
		item.Status = "queued"
		item.Reason = ""
	case "continue_budget", "retry_start":
		limits := s.settings.View().Limits
		if c.Kind == "continue_budget" && item.Status != "paused_budget" {
			return Receipt{}, ErrConflict
		}
		if c.Kind == "retry_start" {
			if item.body.StartLearning == nil || item.body.StartLearning.Result != nil || (item.Status != "partial" && item.Status != "failed") || !item.BodyAvailable {
				return Receipt{}, ErrConflict
			}
			// 显式重试授权新的请求预算；已有成功片段和准备结果继续复用。
			if len(item.body.Research.Sources) == 0 {
				item.body.Research.Discovered = false
			}
			for i := range item.body.Research.Sources {
				if item.body.Research.Sources[i].Status == "failed" {
					item.body.Research.Sources[i].Status = "candidate"
				}
			}
			if item.body.StartLearning.Prepared == nil {
				item.body.Research.Synthesis = nil
			}
			item.callStarted = false
			item.ResultUnknown = false
		}
		if c.RequestBudget < 1 || c.TokenBudget < 1 || c.RequestBudget > limits.ResearchRequests || c.TokenBudget > limits.ResearchTokens {
			return Receipt{}, ErrInvalid
		}
		item.RequestsLeft = c.RequestBudget
		item.TokensLeft = c.TokenBudget
		item.Status = "queued"
		item.Reason = ""
	}
	item.Stage = item.Status
	if err = s.save(ctx, tx, &item, "command"); err != nil {
		return Receipt{}, err
	}
	r, err := receipt(ctx, tx, item, c.OperationID, hash)
	if err != nil {
		return Receipt{}, err
	}
	s.cache(item)
	if err = tx.Commit(ctx); err != nil {
		return Receipt{}, err
	}
	return r, nil
}
