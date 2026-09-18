package mentorrun

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/learningchange"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/platform/keyrotation"
)

func TestPostgreSQLTutorHistoryKeyRotationIncludesSharedOwners(t *testing.T) {
	f := adaptiveFixture(t)
	ctx := context.Background()
	change := f.propose(t, "goal")
	base := f.context(t).Base
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) {
		stream(w, modelclient.Message{Role: "assistant", Content: "轮换后仍能读取"})
	}
	id := newHistory(t, f.runtimeFixture, true, f.goal)
	receipt := sendHistory(t, f.runtimeFixture, id, "轮换测试问题")
	f.work(t)
	oldKey := bytes.Repeat([]byte{42}, 32)
	newKey := bytes.Repeat([]byte{43}, 32)
	rewrap, err := keyrotation.New(oldKey, newKey)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `LOCK TABLE learning_mentor_runs,learning_tutor_conversations,learning_tutor_turns,learning_content_revisions,learning_changes,learning_change_revisions IN ACCESS EXCLUSIVE MODE; SET LOCAL app.mentor_key_rotation='on'`); err != nil {
		t.Fatal(err)
	}
	if err = RotateKeyTx(ctx, tx, rewrap); err != nil {
		t.Fatal(err)
	}
	if err = learningcontent.RotateKeyTx(ctx, tx, rewrap); err != nil {
		t.Fatal(err)
	}
	if err = learningchange.RotateKeyTx(ctx, tx, rewrap); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = f.service.ReadConversation(ctx, f.actor, learningspace.DefaultID, id, 0, 20, func(TurnPage) error { t.Error("旧密钥仍能解密"); return nil }); err == nil {
		t.Fatal("旧密钥未失效")
	}
	restarted, err := New(f.pool, f.settings, newKey)
	if err != nil {
		t.Fatal(err)
	}
	f.service = restarted
	if page := historyPage(t, f.runtimeFixture, id, 0, 20); page.Items[0].Output != "轮换后仍能读取" {
		t.Fatal("轮次轮换丢失正文")
	}
	if snapshot := f.snapshot(t, receipt.RunID); snapshot.Output != "轮换后仍能读取" {
		t.Fatal("checkpoint 轮换丢失正文")
	}
	content, err := learningcontent.New(f.pool, nil, newKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = content.Get(ctx, f.actor, base.ArtifactID, base.ArtifactVersion); err != nil {
		t.Fatal("正式内容未一起轮换", err)
	}
	var raw []byte
	if err = f.pool.QueryRow(ctx, `SELECT ciphertext FROM learning_change_revisions WHERE change_id=$1 AND revision=$2`, change.ID, change.Revision).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	a, _ := newCipher(newKey)
	if _, err = a.Open(nil, raw[:a.NonceSize()], raw[a.NonceSize():], []byte(change.ID)); err != nil {
		t.Fatal("教学变更历史未轮换", err)
	}
	if _, err = f.pool.Exec(ctx, `UPDATE learning_content_revisions SET status='draft' WHERE artifact_id=$1`, base.ArtifactID); err == nil {
		t.Fatal("轮换放开了正常不可变版本保护")
	}
}
