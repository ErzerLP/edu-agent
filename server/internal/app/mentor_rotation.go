package app

import (
	"bytes"
	"context"
	"errors"

	"github.com/edu-agent/edu-agent/server/internal/learningchange"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/edu-agent/edu-agent/server/internal/platform/config"
	"github.com/edu-agent/edu-agent/server/internal/platform/keyrotation"
	platformpostgres "github.com/edu-agent/edu-agent/server/internal/platform/postgres"
	"github.com/edu-agent/edu-agent/server/migrations"
)

func RotateMentorKey(ctx context.Context, cfg config.Config, newPath string) error {
	oldKey, err := mentorrun.LoadKey(cfg.MentorKeyFile)
	if err != nil {
		return err
	}
	defer clear(oldKey)
	newKey, err := mentorrun.LoadKey(newPath)
	if err != nil {
		return err
	}
	defer clear(newKey)
	if bytes.Equal(oldKey, newKey) {
		return errors.New("新旧密钥必须不同")
	}
	rewrap, err := keyrotation.New(oldKey, newKey)
	if err != nil {
		return err
	}
	pool, err := platformpostgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err = migrations.Check(ctx, pool); err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var generation int64
	if err = tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate('learning','write',NULL)`).Scan(&generation); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `LOCK TABLE learning_mentor_processes,learning_mentor_runs,learning_tutor_conversations,learning_tutor_turns,learning_content_artifacts,learning_content_revisions,learning_changes,learning_change_revisions IN ACCESS EXCLUSIVE MODE NOWAIT`); err != nil {
		return errors.New("仍有活动事务，请停止所有服务再轮换")
	}
	var live bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM learning_mentor_processes WHERE live_until>clock_timestamp())`).Scan(&live); err != nil {
		return err
	}
	if live {
		return errors.New("仍有服务心跳，请停止所有实例并等待至少 30 秒")
	}
	if _, err = tx.Exec(ctx, `SET LOCAL app.mentor_key_rotation='on'`); err != nil {
		return err
	}
	if err = mentorrun.RotateKeyTx(ctx, tx, rewrap); err != nil {
		return err
	}
	if err = learningcontent.RotateKeyTx(ctx, tx, rewrap); err != nil {
		return err
	}
	if err = learningchange.RotateKeyTx(ctx, tx, rewrap); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
