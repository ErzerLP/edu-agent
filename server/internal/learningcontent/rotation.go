package learningcontent

import (
	"context"
	"github.com/edu-agent/edu-agent/server/internal/platform/keyrotation"
	"github.com/jackc/pgx/v5"
)

// RotateKeyTx 与导师历史共用服务端密钥；轮换不改变正文版本、状态或来源。
func RotateKeyTx(ctx context.Context, tx pgx.Tx, rewrap keyrotation.Rewrap) error {
	return keyrotation.Rewrite(ctx, tx, `SELECT r.artifact_id||':'||r.version,
	'learningcontent:v1:'||a.id||':'||r.version||':'||a.privacy_generation||':'||a.space_id||':'||a.goal_id||':'||a.goal_revision_id||':'||a.session_id||':'||a.activity_id||':'||r.status||':'||a.activity_revision||':'||r.actor_device_id,r.ciphertext
	FROM learning_content_revisions r JOIN learning_content_artifacts a ON a.id=r.artifact_id WHERE r.ciphertext IS NOT NULL AND r.artifact_id||':'||r.version>$1 ORDER BY r.artifact_id||':'||r.version LIMIT 16`,
		`UPDATE learning_content_revisions SET ciphertext=$2 WHERE artifact_id=split_part($1,':',1)::uuid AND version=split_part($1,':',2)::bigint`, rewrap)
}
