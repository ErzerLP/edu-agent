package learningcontent

import (
	"context"
	"github.com/edu-agent/edu-agent/server/internal/platform/keyrotation"
	"github.com/jackc/pgx/v5"
)

func rotateStudioJobsTx(ctx context.Context, tx pgx.Tx, rewrap keyrotation.Rewrap) error {
	if _, err := tx.Exec(ctx, `LOCK TABLE learning_studio_jobs,learning_studio_plans,learning_studio_plan_messages,learning_studio_job_operations IN ACCESS EXCLUSIVE MODE NOWAIT`); err != nil {
		return err
	}
	for _, q := range []struct{ read, write string }{
		{`SELECT id::text,'studio-job:v1:'||space_id||':'||id||':'||privacy_generation||':input:',ciphertext FROM learning_studio_jobs WHERE id::text>$1 ORDER BY id::text LIMIT 16`, `UPDATE learning_studio_jobs SET ciphertext=$2 WHERE id=$1::uuid`},
		{`SELECT p.job_id||':'||p.revision,'studio-job:v1:'||j.space_id||':'||j.id||':'||j.privacy_generation||':plan:'||p.revision,p.ciphertext FROM learning_studio_plans p JOIN learning_studio_jobs j ON j.id=p.job_id WHERE p.job_id||':'||p.revision>$1 ORDER BY p.job_id||':'||p.revision LIMIT 16`, `UPDATE learning_studio_plans SET ciphertext=$2 WHERE job_id=split_part($1,':',1)::uuid AND revision=split_part($1,':',2)::bigint`},
		{`SELECT m.job_id||':'||m.operation_id,'studio-job:v1:'||j.space_id||':'||j.id||':'||j.privacy_generation||':message:'||m.operation_id,m.ciphertext FROM learning_studio_plan_messages m JOIN learning_studio_jobs j ON j.id=m.job_id WHERE m.job_id||':'||m.operation_id>$1 ORDER BY m.job_id||':'||m.operation_id LIMIT 16`, `UPDATE learning_studio_plan_messages SET ciphertext=$2 WHERE job_id=split_part($1,':',1)::uuid AND operation_id=split_part($1,':',2)::uuid`},
		{`SELECT o.device_id||':'||o.operation_id,'studio-job:v1:'||j.space_id||':'||j.id||':'||j.privacy_generation||':operation:'||o.device_id||':'||o.operation_id,o.ciphertext FROM learning_studio_job_operations o JOIN learning_studio_jobs j ON j.id=o.job_id WHERE o.device_id||':'||o.operation_id>$1 ORDER BY o.device_id||':'||o.operation_id LIMIT 16`, `UPDATE learning_studio_job_operations SET ciphertext=$2 WHERE device_id=split_part($1,':',1)::uuid AND operation_id=split_part($1,':',2)::uuid`},
	} {
		if err := keyrotation.Rewrite(ctx, tx, q.read, q.write, rewrap); err != nil {
			return err
		}
	}
	return nil
}
