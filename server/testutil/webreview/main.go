// webreview 仅在 webdatabase 创建的隔离 schema 中初始化已到期的复习证据。
// 不编入 edu-agentd；当前活动由正式 HTTP 服务创建，历史动作全部经过学习应用服务。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/integrations/learningknowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	device := flag.String("device", "", "正式服务已配对的设备 UUID")
	space := flag.String("space", "", "活动所属学习区 UUID")
	session := flag.String("session", "", "正式服务已生成活动的会话 UUID")
	completed := flag.Bool("completed", false, "完成原会话，要求用户明确新建复习承载")
	flag.Parse()
	if err := run(*device, *space, *session, *completed); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func requireBrowserSchema(schema string) error {
	if !regexp.MustCompile(`^web_candidate_[0-9a-f]{32}$`).MatchString(schema) {
		return fmt.Errorf("历史复习夹具仅允许 webdatabase 的隔离浏览器 schema")
	}
	return nil
}

func run(device, space, session string, completed bool) error {
	if os.Getenv("TEST_DATABASE_URL") == "" || uuid.Validate(device) != nil || uuid.Validate(space) != nil || uuid.Validate(session) != nil || flag.NArg() != 0 {
		return fmt.Errorf("需要 TEST_DATABASE_URL 和有效的 device、space、session")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx, err := learningspace.WithScope(ctx, space)
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	var schema string
	if err = pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		return err
	}
	if err = requireBrowserSchema(schema); err != nil {
		return err
	}
	ids, err := identity.NewService(identitydb.New(pool), identity.Options{PairingCodeTTL: time.Minute, PairingCodeMaxAttempts: 5, LastUsedTouchInterval: time.Minute})
	if err != nil {
		return err
	}
	actor, err := ids.Authenticate(ctx, os.Getenv("WEB_REVIEW_TOKEN"), "learning:write")
	if err != nil {
		return err
	}
	if actor.Device.ID != device {
		return fmt.Errorf("历史初始化设备与凭据不一致")
	}
	key, err := mentorrun.LoadKey(os.Getenv("WEB_REVIEW_KEY_FILE"))
	if err != nil {
		return err
	}
	kstore := knowledgedb.New(pool)
	ks, err := knowledge.NewService(kstore, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		return err
	}
	store := learningdb.New(pool, tutoringdb.New(pool), kstore)
	content, err := learningcontent.New(pool, store, key)
	if err != nil {
		return err
	}
	store.ConfigureContent(content)
	past := time.Now().UTC().Add(-72 * time.Hour)
	svc, err := learning.NewService(store, store, learningknowledge.New(ks), learning.ServiceOptions{
		Now: func() time.Time { return past }, Model: assessmentModel{}, ModelID: "web-review-history-fixture", PromptRevision: "web-review-history-v1",
	})
	if err != nil {
		return err
	}
	view, err := svc.Session(ctx, session)
	if err != nil {
		return err
	}
	if view.Session.State != tutoring.StateActivityIssued || view.WorkItem.Activity == nil {
		return fmt.Errorf("历史复习初始化要求原会话处于 ActivityIssued")
	}
	body, err := content.Ensure(ctx, actor, session, view.WorkItem.Activity.ID)
	if err != nil {
		return err
	}
	apply := func(action tutoring.Action, proposal string) error {
		current, err := svc.Session(ctx, session)
		if err != nil {
			return err
		}
		actionContext := ctx
		if action == tutoring.ActionSubmitAttempt {
			actionContext = content.WithAnswer(ctx, actor, body.ArtifactID, body.Version)
		}
		_, err = svc.ApplyAction(actionContext, device, session, learning.ActionCommand{
			Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: session, ExpectedVersion: current.Session.AggregateVer, Payload: json.RawMessage(`{}`)},
			Action:    action, ProposalID: proposal, Answer: "A", Help: learning.HelpNone,
		})
		return err
	}
	for _, action := range []tutoring.Action{tutoring.ActionPresentActivity, tutoring.ActionSubmitAttempt} {
		if err = apply(action, ""); err != nil {
			return fmt.Errorf("历史动作 %s：%w", action, err)
		}
	}
	current, err := svc.Session(ctx, session)
	if err != nil {
		return err
	}
	proposalID := ""
	if current.WorkItem.Activity.Type == learning.ActivityOpen {
		// 模型请求的租约由 PG 真实时钟校验；只回拨学习动作，不回拨请求租约。
		proposer, err := learning.NewService(store, store, learningknowledge.New(ks), learning.ServiceOptions{Model: assessmentModel{}, ModelID: "web-review-history-fixture", PromptRevision: "web-review-history-v1"})
		if err != nil {
			return err
		}
		input, err := json.Marshal(current.WorkItem)
		if err != nil {
			return err
		}
		proposal, err := proposer.Propose(ctx, device, learning.ProposalRequest{RequestID: uuid.NewString(), Type: learning.ProposalAssessment, AggregateType: "session", AggregateID: session, AggregateVersion: current.Session.AggregateVer, KnowledgeRevisionID: current.WorkItem.Activity.KnowledgeRevisionID, NodeRevisionIDs: []string{current.WorkItem.Activity.TargetNodeRevisionID}, Input: input})
		if err != nil {
			return err
		}
		proposalID = proposal.ID
	}
	if err = apply(tutoring.ActionRecordAssessment, proposalID); err != nil {
		return err
	}
	feedback, err := svc.Feedback(ctx, current.WorkItem.Attempt.ID, false)
	if err != nil {
		return err
	}
	if len(feedback.Evidence) != 1 || feedback.Evidence[0].Outcome != learning.OutcomePass {
		return fmt.Errorf("历史答案没有形成一条正式通过证据")
	}
	// 结束活动保留原节点，可进入原会话复习；完成会话只能从 RouteActive 发起。
	if err = apply(tutoring.ActionEndActivity, ""); err != nil {
		return err
	}
	if completed {
		if err = apply(tutoring.ActionCompleteSession, ""); err != nil {
			return err
		}
	}
	return json.NewEncoder(os.Stdout).Encode(feedback)
}

// 开放题初始证据仍经过原评估引用、字节范围和摘要核验，不直接接纳 Evidence。
type assessmentModel struct{}

func (assessmentModel) Generate(_ context.Context, request learning.ProposalRequest) (json.RawMessage, error) {
	var item learning.SessionWorkItem
	if err := json.Unmarshal(request.Input, &item); err != nil {
		return nil, err
	}
	ref := item.Activity.References[0]
	answer := item.Attempt.Answer
	hash := func(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }
	return json.Marshal(map[string]any{"assessment": map[string]any{
		"items":           []learning.AssessmentItem{{RubricItemID: item.Activity.Rubric.Items[0].ID, Conclusion: learning.ConclusionPass, AnswerQuote: answer, AnswerRange: learning.SourceRange{Start: 0, End: len(answer)}, AnswerQuoteSHA256: hash(answer), KnowledgeReferenceID: ref.NodeRevisionID, KnowledgeQuote: ref.Slice, KnowledgeRange: learning.SourceRange{Start: 0, End: len(ref.Slice)}, KnowledgeQuoteSHA256: hash(ref.Slice)}},
		"rubric_complete": true, "confidence": 1000, "risk_flags": []string{},
	}})
}
