package mentorrun

import (
	"context"
	"github.com/edu-agent/edu-agent/server/internal/platform/keyrotation"
	"github.com/jackc/pgx/v5"
)

// RotateKeyTx 由停机维护组合根在同一事务内调用，保留历史格式及运行状态。
func RotateKeyTx(ctx context.Context, tx pgx.Tx, rewrap keyrotation.Rewrap) error {
	for _, table := range []struct{ query, update string }{
		{`SELECT id::text,id::text,checkpoint FROM learning_mentor_runs WHERE checkpoint IS NOT NULL AND id::text>$1 ORDER BY id::text LIMIT 16`, `UPDATE learning_mentor_runs SET checkpoint=$2 WHERE id=$1::uuid`},
		{`SELECT id::text,'tutor:'||id||':'||privacy_generation||':title',title FROM learning_tutor_conversations WHERE title IS NOT NULL AND id::text>$1 ORDER BY id::text LIMIT 16`, `UPDATE learning_tutor_conversations SET title=$2 WHERE id=$1::uuid`},
		{`SELECT t.run_id::text,'tutor:'||c.id||':'||c.privacy_generation||':'||t.run_id,t.ciphertext FROM learning_tutor_turns t JOIN learning_tutor_conversations c ON c.id=t.conversation_id WHERE t.ciphertext IS NOT NULL AND t.run_id::text>$1 ORDER BY t.run_id::text LIMIT 16`, `UPDATE learning_tutor_turns SET ciphertext=$2 WHERE run_id=$1::uuid`},
	} {
		if err := keyrotation.Rewrite(ctx, tx, table.query, table.update, rewrap); err != nil {
			return err
		}
	}
	return nil
}
