package postgresstore_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	postgres "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/google/uuid"
)

type lostJobResponseStore struct {
	*postgres.Store
	lose bool
}

func (s *lostJobResponseStore) CommitImport(ctx context.Context, c knowledge.PreparedCommit) (knowledge.ImportResult, error) {
	r, e := s.Store.CommitImport(ctx, c)
	if e == nil && s.lose {
		s.lose = false
		return knowledge.ImportResult{}, errors.New("模拟提交后连接断开")
	}
	return r, e
}

func TestPostgreSQLImportJobRestartLostResponseAndLargeManifest(t *testing.T) {
	ctx, pool, store, _ := newReviewerPostgresHarness(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	postgres.WithImportJobDirectory(dir)(store)
	lossy := &lostJobResponseStore{Store: store, lose: true}
	s, _ := knowledge.NewService(lossy, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	id := uuid.NewString()
	items := []knowledge.ImportJobItem{}
	docs := []knowledge.ImportDocument{}
	for i := 0; i < 5; i++ {
		d := knowledge.ImportDocument{Path: fmt.Sprintf("%d.md", i), Markdown: fmt.Sprintf("# 文件 %d\n", i) + strings.Repeat(fmt.Sprintf("内容%d ", i), 450000)}
		sum := sha256.Sum256([]byte(d.Markdown))
		items = append(items, knowledge.ImportJobItem{Path: d.Path, Bytes: len(d.Markdown), Digest: hex.EncodeToString(sum[:])})
		docs = append(docs, d)
	}
	total := 0
	for _, i := range items {
		total += i.Bytes
	}
	if total <= 16<<20 {
		t.Fatal("夹具未超过单请求预算")
	}
	run := func(c knowledge.ImportJobCommand) knowledge.ImportJob {
		t.Helper()
		c.ID = id
		j, e := s.RunImportJob(ctx, integrationActorID, c)
		if e != nil {
			t.Fatalf("%s: %v", c.Action, e)
		}
		return j
	}
	j := run(knowledge.ImportJobCommand{Action: "create", Items: items})
	for i := range docs {
		j = run(knowledge.ImportJobCommand{Action: "upload", Batch: i, Document: &docs[i]})
	}
	files, e := os.ReadDir(dir)
	if e != nil || len(files) != 5 {
		t.Fatalf("暂存文件: %d %v", len(files), e)
	}
	raw, e := os.ReadFile(dir + "/" + files[0].Name())
	if e != nil || strings.Contains(string(raw), "内容") {
		t.Fatal("暂存未加密")
	}
	// 新服务实例与同一数据库、目录恢复，不依赖旧 HTTP 连接。
	s, _ = knowledge.NewService(lossy, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	j = run(knowledge.ImportJobCommand{Action: "preview"})
	if j.Status != "ready" {
		t.Fatalf("预览: %+v", j.Batches[0])
	}
	if head, e := store.Head(ctx); e != nil || head != nil {
		t.Fatal("预览发布了版本")
	}
	j = run(knowledge.ImportJobCommand{Action: "confirm", PlanVersion: j.PlanVersion})
	j = run(knowledge.ImportJobCommand{Action: "continue"})
	if j.Batches[0].Status != "unknown" {
		t.Fatalf("响应丢失: %s", j.Batches[0].Status)
	}
	s, _ = knowledge.NewService(lossy, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	j = run(knowledge.ImportJobCommand{Action: "get"})
	if j.Batches[0].Status != "completed" {
		t.Fatal("未对账成功提交")
	}
	for j.Status != "completed" {
		j = run(knowledge.ImportJobCommand{Action: "continue"})
		if j.Status == "review" || j.Status == "failed" {
			t.Fatalf("继续失败: %+v", j)
		}
	}
	var count int
	if e = pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_import_operations`).Scan(&count); e != nil || count != 5 {
		t.Fatalf("操作重复: %d %v", count, e)
	}
	files, e = os.ReadDir(dir)
	if e != nil || len(files) != 0 {
		t.Fatalf("完成未清理: %d %v", len(files), e)
	}
	if _, e = s.RunImportJob(ctx, uuid.NewString(), knowledge.ImportJobCommand{ID: id, Action: "get"}); knowledge.ErrorCode(e) != knowledge.CodeNotFound {
		t.Fatalf("跨设备读取: %v", e)
	}
}

func TestPostgreSQLImportJobCancelStaleAndExpiry(t *testing.T) {
	ctx, _, store, s := newReviewerPostgresHarness(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	postgres.WithImportJobDirectory(dir)(store)
	makeJob := func() knowledge.ImportJob {
		t.Helper()
		id := uuid.NewString()
		items := []knowledge.ImportJobItem{}
		docs := []knowledge.ImportDocument{{Path: id + "a.md", Markdown: "# First\nalpha"}, {Path: id + "b.md", Markdown: "# Second\nbeta"}}
		for _, d := range docs {
			sum := sha256.Sum256([]byte(d.Markdown))
			items = append(items, knowledge.ImportJobItem{Path: d.Path, Bytes: len(d.Markdown), Digest: hex.EncodeToString(sum[:])})
		}
		j, e := s.RunImportJob(ctx, integrationActorID, knowledge.ImportJobCommand{ID: id, Action: "create", Items: items})
		if e != nil {
			t.Fatal(e)
		}
		for i := range docs {
			j, e = s.RunImportJob(ctx, integrationActorID, knowledge.ImportJobCommand{ID: id, Action: "upload", Batch: i, Document: &docs[i]})
			if e != nil {
				t.Fatal(e)
			}
		}
		j, e = s.RunImportJob(ctx, integrationActorID, knowledge.ImportJobCommand{ID: id, Action: "preview"})
		if e != nil || j.Status != "ready" {
			t.Fatalf("preview: %s %v", j.Status, e)
		}
		return j
	}
	j := makeJob()
	j, e := s.RunImportJob(ctx, integrationActorID, knowledge.ImportJobCommand{ID: j.ID, Action: "confirm", PlanVersion: j.PlanVersion})
	if e != nil {
		t.Fatal(e)
	}
	j, e = s.RunImportJob(ctx, integrationActorID, knowledge.ImportJobCommand{ID: j.ID, Action: "continue"})
	if e != nil || j.Batches[0].Status != "completed" {
		t.Fatal(e)
	}
	j, e = s.RunImportJob(ctx, integrationActorID, knowledge.ImportJobCommand{ID: j.ID, Action: "cancel"})
	if e != nil || j.Status != "cancelled" || j.Batches[0].Result == nil || j.Batches[1].Result != nil {
		t.Fatalf("cancel: %+v %v", j, e)
	}
	if _, e = s.RunImportJob(ctx, integrationActorID, knowledge.ImportJobCommand{ID: j.ID, Action: "continue"}); knowledge.ErrorCode(e) != "import_job_closed" {
		t.Fatal(e)
	}
	// 过期由服务端时间决定，查询回收密钥；成功版本不受影响。
	s, _ = knowledge.NewService(store, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{Now: func() time.Time { return time.Now().Add(25 * time.Hour) }})
	j, e = s.RunImportJob(ctx, integrationActorID, knowledge.ImportJobCommand{ID: j.ID, Action: "get"})
	if e != nil || j.Status != "cancelled" {
		t.Fatal(e)
	}
}

func TestPostgreSQLImportJobExternalWriteMissingStagingAndQuota(t *testing.T) {
	ctx, pool, store, s := newReviewerPostgresHarness(t)
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	postgres.WithImportJobDirectory(dir)(store)
	id := uuid.NewString()
	d := knowledge.ImportDocument{Path: "incoming.md", Markdown: "# New incoming\nunique pending job content"}
	sum := sha256.Sum256([]byte(d.Markdown))
	items := []knowledge.ImportJobItem{{Path: d.Path, Bytes: len(d.Markdown), Digest: hex.EncodeToString(sum[:])}}
	run := func(c knowledge.ImportJobCommand) knowledge.ImportJob {
		t.Helper()
		c.ID = id
		j, e := s.RunImportJob(ctx, integrationActorID, c)
		if e != nil {
			t.Fatalf("%s: %v", c.Action, e)
		}
		return j
	}
	run(knowledge.ImportJobCommand{Action: "create", Items: items})
	run(knowledge.ImportJobCommand{Action: "upload", Document: &d})
	j := run(knowledge.ImportJobCommand{Action: "preview"})
	oldPlan := j.PlanVersion
	run(knowledge.ImportJobCommand{Action: "confirm", PlanVersion: oldPlan})
	if _, e := s.Import(ctx, knowledge.ImportCommand{OperationID: uuid.NewString(), ExpectedParentProvided: true, Source: "外部更新", ActorDeviceID: integrationActorID, Documents: []knowledge.ImportDocument{{Path: "external.md", Markdown: "# External\nunrelated update"}}}); e != nil {
		t.Fatal(e)
	}
	j = run(knowledge.ImportJobCommand{Action: "continue"})
	if j.Status != "review" || j.Approved || j.Batches[0].Result != nil {
		t.Fatalf("外部版本变化未撤销批准: %+v", j)
	}
	j = run(knowledge.ImportJobCommand{Action: "preview"})
	if j.Status != "ready" || j.PlanVersion == oldPlan {
		t.Fatalf("未重新预览: %+v", j)
	}
	if _, e := s.RunImportJob(ctx, integrationActorID, knowledge.ImportJobCommand{ID: id, Action: "confirm", PlanVersion: oldPlan}); knowledge.ErrorCode(e) != knowledge.CodeImportPreviewStale {
		t.Fatalf("旧确认可重用: %v", e)
	}
	if e := os.Remove(dir + "/" + id + "-0.enc"); e != nil {
		t.Fatal(e)
	}
	j = run(knowledge.ImportJobCommand{Action: "preview"})
	if j.Status != "failed" || j.Batches[0].Status != "missing" {
		t.Fatalf("暂存丢失: %+v", j)
	}
	changed := d
	changed.Markdown += "changed"
	if _, e := s.RunImportJob(ctx, integrationActorID, knowledge.ImportJobCommand{ID: id, Action: "upload", Document: &changed}); knowledge.ErrorCode(e) != "import_job_source_changed" {
		t.Fatal(e)
	}
	run(knowledge.ImportJobCommand{Action: "upload", Document: &d})
	// 后台回收使用真实数据库过期时间；任务查询仍呈现过期状态。
	if _, e := pool.Exec(ctx, `UPDATE knowledge_import_jobs SET state=jsonb_set(state,'{expires_at}',to_jsonb($2::text)) WHERE id=$1`, id, time.Now().Add(-time.Hour).Format(time.RFC3339Nano)); e != nil {
		t.Fatal(e)
	}
	if _, e := store.SweepImportJobs(ctx); e != nil {
		t.Fatal(e)
	}
	j = run(knowledge.ImportJobCommand{Action: "get"})
	if j.Status != "expired" {
		t.Fatalf("过期状态: %s", j.Status)
	}
	if files, e := os.ReadDir(dir); e != nil || len(files) != 0 {
		t.Fatal("过期未清理")
	}
	// 完整清单超配额时不创建任务。
	large := make([]knowledge.ImportJobItem, 33)
	for i := range large {
		large[i] = knowledge.ImportJobItem{Path: fmt.Sprintf("%d.md", i), Bytes: 4 << 20, Digest: items[0].Digest}
	}
	if _, e := s.RunImportJob(ctx, integrationActorID, knowledge.ImportJobCommand{ID: uuid.NewString(), Action: "create", Items: large}); knowledge.ErrorCode(e) != "import_job_quota" {
		t.Fatalf("未执行任务配额: %v", e)
	}
}

func TestPostgreSQLImportJobCleanupFailureAndConcurrentRequest(t *testing.T) {
	ctx, pool, store, s := newReviewerPostgresHarness(t)
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	postgres.WithImportJobDirectory(dir)(store)
	d := knowledge.ImportDocument{Path: "source.md", Markdown: "# source"}
	sum := sha256.Sum256([]byte(d.Markdown))
	id := uuid.NewString()
	if _, e := s.RunImportJob(ctx, integrationActorID, knowledge.ImportJobCommand{ID: id, Action: "create", Items: []knowledge.ImportJobItem{{Path: d.Path, Bytes: len(d.Markdown), Digest: hex.EncodeToString(sum[:])}}}); e != nil {
		t.Fatal(e)
	}
	if _, e := s.RunImportJob(ctx, integrationActorID, knowledge.ImportJobCommand{ID: id, Action: "upload", Document: &d}); e != nil {
		t.Fatal(e)
	}
	unlock, e := store.LockImportJob(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.RunImportJob(ctx, integrationActorID, knowledge.ImportJobCommand{ID: id, Action: "cancel"}); knowledge.ErrorCode(e) != "import_job_busy" {
		unlock()
		t.Fatalf("并发取消穿过批次锁: %v", e)
	}
	unlock()
	path := dir + "/" + id + "-0.enc"
	if e = os.Remove(path); e != nil {
		t.Fatal(e)
	}
	if e = os.Mkdir(path, 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path+"/block", []byte("测试清理失败"), 0600); e != nil {
		t.Fatal(e)
	}
	j, e := s.RunImportJob(ctx, integrationActorID, knowledge.ImportJobCommand{ID: id, Action: "cancel"})
	if e != nil || !j.CleanupPending || j.Status != "cancelled" {
		t.Fatalf("删除失败未呈现: %+v %v", j, e)
	}
	var destroyed bool
	if e = pool.QueryRow(ctx, `SELECT staging_key IS NULL FROM knowledge_import_jobs WHERE id=$1`, id).Scan(&destroyed); e != nil || !destroyed {
		t.Fatal("文件删除失败时密钥未清除")
	}
	if e = os.Remove(path + "/block"); e != nil {
		t.Fatal(e)
	}
	j, e = s.RunImportJob(ctx, integrationActorID, knowledge.ImportJobCommand{ID: id, Action: "get"})
	if e != nil || j.CleanupPending {
		t.Fatalf("清理不能恢复: %+v %v", j, e)
	}
}
