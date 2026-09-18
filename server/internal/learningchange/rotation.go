package learningchange

import (
	"context"
	"github.com/edu-agent/edu-agent/server/internal/platform/keyrotation"
	"github.com/jackc/pgx/v5"
)

// RotateKeyTx 覆盖候选及其历史版本；旧的短期参考预览票据在换钥后失效，须重新预览。
func RotateKeyTx(ctx context.Context, tx pgx.Tx, rewrap keyrotation.Rewrap) error {
	if err := keyrotation.Rewrite(ctx, tx, `SELECT id::text,id::text,ciphertext FROM learning_changes WHERE ciphertext IS NOT NULL AND id::text>$1 ORDER BY id::text LIMIT 16`, `UPDATE learning_changes SET ciphertext=$2 WHERE id=$1::uuid`, rewrap); err != nil {
		return err
	}
	return keyrotation.Rewrite(ctx, tx, `SELECT change_id||':'||revision,change_id::text,ciphertext FROM learning_change_revisions WHERE ciphertext IS NOT NULL AND change_id||':'||revision>$1 ORDER BY change_id||':'||revision LIMIT 16`, `UPDATE learning_change_revisions SET ciphertext=$2 WHERE change_id=split_part($1,':',1)::uuid AND revision=split_part($1,':',2)::bigint`, rewrap)
}
