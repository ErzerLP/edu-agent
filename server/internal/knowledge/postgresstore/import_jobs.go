package postgresstore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func WithImportJobDirectory(path string) Option {
	return func(s *Store) { s.importJobDirectory = path }
}
func jobFailure(code string) error { return &knowledge.Error{Code: code} }
func (s *Store) LockImportJob(ctx context.Context, id string) (func(), error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	var locked bool
	err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,717))`, id).Scan(&locked)
	if err != nil || !locked {
		_ = tx.Rollback(context.Background())
		if err != nil {
			return nil, err
		}
		return nil, jobFailure("import_job_busy")
	}
	return func() { _ = tx.Rollback(context.Background()) }, nil
}
func (s *Store) CreateImportJob(ctx context.Context, j knowledge.ImportJob) (knowledge.ImportJob, error) {
	if old, err := s.LoadImportJob(ctx, j.ID, j.Actor); err == nil {
		if len(old.Batches) != len(j.Batches) {
			return old, jobFailure(knowledge.CodeIdempotencyConflict)
		}
		for i := range old.Batches {
			if old.Batches[i].Item != j.Batches[i].Item {
				return old, jobFailure(knowledge.CodeIdempotencyConflict)
			}
		}
		return old, nil
	} else {
		var domain *knowledge.Error
		if !errors.As(err, &domain) || domain.Code != knowledge.CodeNotFound {
			return j, err
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return j, err
	}
	defer tx.Rollback(context.Background())
	generation, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge)
	if err != nil {
		return j, err
	}
	if generation != j.Generation {
		return j, jobFailure("import_preview_stale")
	}
	if err = lockCollectionWrite(ctx, tx); err != nil {
		return j, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(717,1)`); err != nil {
		return j, err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM knowledge_import_jobs WHERE staging_key IS NOT NULL`).Scan(&count); err != nil {
		return j, err
	}
	if count >= 32 {
		return j, jobFailure("import_job_quota")
	}
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return j, err
	}
	raw, _ := json.Marshal(j)
	tag, err := tx.Exec(ctx, `INSERT INTO knowledge_import_jobs(id,actor_id,space_id,collection_id,generation,version,state,staging_key) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(id) DO NOTHING`, j.ID, j.Actor, j.Space, j.Collection, j.Generation, j.Version, raw, key)
	if err != nil {
		return j, err
	}
	if tag.RowsAffected() != 1 {
		return j, jobFailure(knowledge.CodeIdempotencyConflict)
	}
	return j, tx.Commit(ctx)
}
func (s *Store) LoadImportJob(ctx context.Context, id, actor string) (knowledge.ImportJob, error) {
	tx, err := s.beginPrivacyRead(ctx)
	if err != nil {
		return knowledge.ImportJob{}, err
	}
	defer tx.Rollback(context.Background())
	if err = checkCollection(ctx, tx, knowledge.CollectionID(ctx)); err != nil {
		return knowledge.ImportJob{}, err
	}
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT state FROM knowledge_import_jobs WHERE id=$1 AND actor_id=$2 AND space_id=$3 AND collection_id=$4 AND state<>'{}'::jsonb AND generation=(SELECT learner_generation FROM privacy_owner_generation_gates WHERE owner_kind='knowledge')`, id, actor, learningspace.Scope(ctx), knowledge.CollectionID(ctx)).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return knowledge.ImportJob{}, jobFailure(knowledge.CodeNotFound)
	}
	var j knowledge.ImportJob
	if err == nil {
		err = json.Unmarshal(raw, &j)
	}
	if err != nil {
		return j, err
	}
	return j, tx.Commit(ctx)
}
func (s *Store) ListImportJobs(ctx context.Context, actor, cursor string) (knowledge.ImportJobPage, error) {
	page := knowledge.ImportJobPage{Items: []knowledge.ImportJob{}}
	tx, err := s.beginPrivacyRead(ctx)
	if err != nil {
		return page, err
	}
	defer tx.Rollback(context.Background())
	if err = checkCollection(ctx, tx, knowledge.CollectionID(ctx)); err != nil {
		return page, err
	}
	rows, err := tx.Query(ctx, `SELECT (state-'batches')||jsonb_build_object('batch_count',jsonb_array_length(state->'batches')) FROM knowledge_import_jobs WHERE actor_id=$1 AND space_id=$2 AND collection_id=$3 AND ($4='' OR id>NULLIF($4,'')::uuid) AND state<>'{}'::jsonb AND generation=(SELECT learner_generation FROM privacy_owner_generation_gates WHERE owner_kind='knowledge') ORDER BY id LIMIT 101`, actor, learningspace.Scope(ctx), knowledge.CollectionID(ctx), cursor)
	if err != nil {
		return page, err
	}
	result := []knowledge.ImportJob{}
	for rows.Next() {
		var raw []byte
		var j knowledge.ImportJob
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return page, err
		}
		if err = json.Unmarshal(raw, &j); err != nil {
			rows.Close()
			return page, err
		}
		result = append(result, j)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(result) > 100 {
		result = result[:100]
		page.NextCursor = result[99].ID
	}
	page.Items = result
	return page, tx.Commit(ctx)
}
func (s *Store) SaveImportJob(ctx context.Context, j *knowledge.ImportJob) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	generation, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge)
	if err != nil {
		return err
	}
	if generation != j.Generation {
		return jobFailure("import_preview_stale")
	}
	if err = checkCollection(ctx, tx, j.Collection); err != nil {
		return err
	}
	previous := j.Version
	j.Version++
	raw, _ := json.Marshal(j)
	tag, err := tx.Exec(ctx, `UPDATE knowledge_import_jobs SET state=$2,version=$3 WHERE id=$1 AND version=$4 AND generation=$5 AND state<>'{}'::jsonb`, j.ID, raw, j.Version, previous, j.Generation)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return jobFailure("import_job_busy")
	}
	return tx.Commit(ctx)
}
func (s *Store) jobRoot() (*os.Root, error) {
	path := s.importJobDirectory
	if path == "" {
		path = os.Getenv("IMPORT_JOB_STAGING_DIR")
	}
	if path == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(base, "edu-agent", "import-jobs")
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("导入暂存目录必须为绝对路径")
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, jobFailure("import_job_storage_unavailable")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, jobFailure("import_job_storage_unavailable")
	}
	return os.OpenRoot(path)
}
func jobFile(j knowledge.ImportJob, batch int) string {
	return j.ID + "-" + strconv.Itoa(batch) + ".enc"
}
func (s *Store) jobCipher(ctx context.Context, j knowledge.ImportJob) (cipher.AEAD, error) {
	tx, err := s.beginPrivacyRead(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	var key []byte
	err = tx.QueryRow(ctx, `SELECT staging_key FROM knowledge_import_jobs WHERE id=$1 AND generation=$2 AND generation=(SELECT learner_generation FROM privacy_owner_generation_gates WHERE owner_kind='knowledge')`, j.ID, j.Generation).Scan(&key)
	if err != nil || len(key) != 32 {
		return nil, jobFailure("import_job_staging_missing")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return aead, tx.Commit(ctx)
}
func (s *Store) PutImportJobPayload(ctx context.Context, j knowledge.ImportJob, batch int, raw []byte) error {
	if len(raw) > 32<<20 {
		return jobFailure(knowledge.CodePayloadTooLarge)
	}
	aead, err := s.jobCipher(ctx, j)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	name := jobFile(j, batch)
	encrypted := aead.Seal(nonce, nonce, raw, []byte(name))
	root, err := s.jobRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	temp := uuid.NewString() + ".tmp"
	file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return jobFailure("import_job_storage_unavailable")
	}
	defer root.Remove(temp)
	_, err = file.Write(encrypted)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = root.Rename(temp, name)
	}
	if err != nil {
		return jobFailure("import_job_storage_unavailable")
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (s *Store) ImportJobPayload(ctx context.Context, j knowledge.ImportJob, batch int) ([]byte, error) {
	aead, err := s.jobCipher(ctx, j)
	if err != nil {
		return nil, err
	}
	root, err := s.jobRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	name := jobFile(j, batch)
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 33<<20 {
		return nil, jobFailure("import_job_staging_missing")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, jobFailure("import_job_staging_missing")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() {
		return nil, jobFailure("import_job_staging_missing")
	}
	raw, err := io.ReadAll(io.LimitReader(file, (33<<20)+1))
	if len(raw) > 33<<20 {
		return nil, jobFailure("import_job_staging_missing")
	}
	if err != nil || len(raw) < aead.NonceSize() {
		return nil, jobFailure("import_job_staging_missing")
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(name))
	if err != nil {
		return nil, jobFailure("import_job_staging_missing")
	}
	return plain, nil
}
func (s *Store) CleanImportJob(ctx context.Context, j *knowledge.ImportJob) error {
	// 先清除密钥，文件删除失败也不能恢复已清除正文。
	if _, err := s.pool.Exec(ctx, `UPDATE knowledge_import_jobs SET staging_key=NULL WHERE id=$1`, j.ID); err != nil {
		return err
	}
	j.CleanupPending = false
	root, err := s.jobRoot()
	if err != nil {
		j.CleanupPending = true
	} else {
		defer root.Close()
		for i := range j.Batches {
			if err = root.Remove(jobFile(*j, i)); err != nil && !errors.Is(err, os.ErrNotExist) {
				j.CleanupPending = true
			}
		}
	}
	for i := range j.Batches {
		j.Batches[i].Preview = nil
	}
	return s.SaveImportJob(ctx, j)
}

// 复用应用已有定时循环，只回收暂存；不会扫描客户端，也不会擅自发布任务。
func (s *Store) SweepImportJobs(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT state FROM knowledge_import_jobs WHERE state<>'{}'::jsonb AND ((staging_key IS NOT NULL AND (state->>'expires_at')::timestamptz<=now()) OR (state->>'cleanup_pending')::boolean) LIMIT 32`)
	if err != nil {
		return 0, err
	}
	var jobs []knowledge.ImportJob
	for rows.Next() {
		var raw []byte
		var j knowledge.ImportJob
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return 0, err
		}
		if err = json.Unmarshal(raw, &j); err != nil {
			rows.Close()
			return 0, err
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, err
	}
	service, _ := knowledge.NewService(s, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	for _, j := range jobs {
		scoped, _ := learningspace.WithScope(ctx, j.Space)
		scoped, _ = knowledge.WithCollection(scoped, j.Collection)
		if _, e := service.RunImportJob(scoped, j.Actor, knowledge.ImportJobCommand{ID: j.ID, Action: "get"}); e != nil {
			if knowledge.ErrorCode(e) == "import_job_busy" {
				continue
			}
			return 0, e
		}
	}
	// 留存七天后只保留幂等墓碑，旧任务ID无法重新创建正文。
	if _, err = s.pool.Exec(ctx, `UPDATE knowledge_import_jobs SET state='{}' WHERE staging_key IS NULL AND state<>'{}'::jsonb AND (state->>'expires_at')::timestamptz<now()-interval '7 days'`); err != nil {
		return 0, err
	}
	root, err := s.jobRoot()
	if err != nil {
		return 0, err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	removed := 0
	for {
		entries, readErr := dir.ReadDir(256)
		for _, entry := range entries {
			name := entry.Name()
			remove := false
			if strings.HasSuffix(name, ".tmp") {
				info, e := entry.Info()
				remove = e == nil && time.Since(info.ModTime()) > time.Hour
			} else if len(name) > 37 && strings.HasSuffix(name, ".enc") {
				id := name[:36]
				if _, e := uuid.Parse(id); e != nil {
					continue
				}
				var live bool
				if e := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM knowledge_import_jobs WHERE id=$1 AND staging_key IS NOT NULL)`, id).Scan(&live); e != nil {
					return removed, e
				}
				remove = !live
			}
			if remove {
				if e := root.Remove(name); e != nil && !errors.Is(e, os.ErrNotExist) {
					return removed, jobFailure("import_job_cleanup_failed")
				}
				removed++
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return removed, readErr
		}
	}
	return removed, nil
}
