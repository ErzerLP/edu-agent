package mentorrun

import (
	"context"
	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/jackc/pgx/v5"
	"time"
)

// The answer remains history, while quotation payloads and retrieval state
// expire with the original run. Expired identifiers cannot resolve to content.
func expireWebEvidence(body *Body) {
	if body.WebExpiresAt == nil || time.Now().Before(*body.WebExpiresAt) {
		return
	}
	body.Web = nil
	for i := range body.Citations {
		c := &body.Citations[i]
		if c.Web != nil {
			copy := *c.Web
			copy.Quote = ""
			c.Web = &copy
			c.Expired = true
		}
	}
	body.ContextNotice = "网页内容已过期；历史网络引用不再提供正文。"
	messages := []modelclient.Message{}
	for _, m := range body.Messages {
		if m.Role == "user" {
			messages = append(messages, m)
		}
	}
	if body.Output != "" {
		messages = append(messages, modelclient.Message{Role: "assistant", Content: body.Output})
	}
	body.Messages = messages
	body.TurnMessages = nil
}
func (s *Service) expireWebHistoryTx(ctx context.Context, tx pgx.Tx, item row) error {
	if item.ConversationID == "" || !item.Saved {
		return nil
	}
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT ciphertext FROM learning_tutor_turns WHERE run_id=$1`, item.RunID).Scan(&raw)
	if err == pgx.ErrNoRows || raw == nil {
		return nil
	}
	if err != nil {
		return err
	}
	var body Body
	if err = openHistory(s.aead, historyAAD(item.ConversationID, item.Generation, item.RunID), raw, &body); err != nil {
		return err
	}
	if body.WebExpiresAt == nil {
		return nil
	}
	expireWebEvidence(&body)
	raw, err = sealHistory(s.aead, historyAAD(item.ConversationID, item.Generation, item.RunID), body)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE learning_tutor_turns SET ciphertext=$2 WHERE run_id=$1`, item.RunID, raw)
	return err
}
