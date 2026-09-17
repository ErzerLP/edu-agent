package learningchange

import (
	"context"
	"errors"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"time"
)

func (s *Service) candidate(ctx context.Context, tx pgx.Tx, c *Change, snap Snapshot, input Candidate) error {
	if input.Kind == "route" && input.ContextID == "" && snap.AvailableContextID != "" {
		input.ContextID = snap.AvailableContextID
	}
	if input.Steps == nil {
		input.Steps = []Step{}
	}
	if input.EvidenceIDs == nil {
		input.EvidenceIDs = []string{}
	}
	for i := range input.Steps {
		if input.Steps[i].Prerequisites == nil {
			input.Steps[i].Prerequisites = []int{}
		}
	}
	if err := input.Validate(); err != nil {
		return err
	}
	evidence := map[string]bool{}
	for _, e := range snap.Evidence {
		evidence[e.ID] = true
	}
	for _, id := range input.EvidenceIDs {
		if !evidence[id] {
			return ErrInvalid
		}
	}
	// 资料范围属于知识政策，不能借目标字段编辑绕过来源授权。
	if input.Goal != nil && input.Goal.ScopeSnapshotID != snap.Goal.GoalManagement().Details.ScopeSnapshotID {
		return ErrInvalid
	}
	c.Candidate = input
	c.Base = snap.Base
	c.Diff = Diff{BeforeSteps: snap.Steps, AfterSteps: snap.Steps, BeforeGoal: snap.Goal.GoalManagement().Details, AfterGoal: snap.Goal.GoalManagement().Details, BeforeContent: []learningcontent.Block{}, AfterContent: []learningcontent.Block{}}
	c.Risk, c.Policy, c.Status = "route", "boundary", "proposed"
	c.Impact = "只改变后续建议；已提交答案、评分标准和学习证据保留。新增步骤不代表能力下降，跳过不代表掌握。"
	if snap.Content != nil {
		c.Diff.BeforeContent = snap.Content.Body.Blocks
		c.Diff.AfterContent = snap.Content.Body.Blocks
	}
	switch input.Kind {
	case "goal":
		c.Risk, c.Policy, c.Status = "goal_scope", "approval", "waiting_approval"
		c.Diff.AfterGoal = *input.Goal
	case "explanation":
		if snap.Content == nil {
			return ErrInvalid
		}
		c.Risk, c.Policy = "presentation", "direct"
		c.Diff.AfterContent = append(append([]learningcontent.Block{}, c.Diff.BeforeContent...), learningcontent.Block{ID: uuid.NewString(), Kind: "callout", Text: input.Explanation, Fallback: input.Explanation})
	case "route":
		c.Diff.AfterSteps = input.Steps
		if snap.Mode.Mode == "cautious" {
			c.Policy, c.Status = "approval", "waiting_approval"
		}
		refs, _, err := s.references(ctx, tx, *c, snap)
		if err != nil {
			return err
		}
		if len(input.Steps) == 0 || len(refs) != len(input.Steps) {
			c.Status = "needs_sources"
			c.Reason = "缺少已授权的来源；补充资料后重新生成候选。"
		}
	}
	c.InteractionID = uuid.NewString()
	c.Hash = candidateHash(*c)
	return nil
}

// ChangeTx 供持有运行租约的导师在同一外层事务内调用；不调用模型或检索网络。
func (s *Service) ChangeTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, goal, session, id string, cmd Command) (Change, error) {
	var c Change
	if !s.Available() {
		return c, ErrUnavailable
	}
	if uuid.Validate(goal) != nil || uuid.Validate(session) != nil || uuid.Validate(id) != nil || uuid.Validate(cmd.OperationID) != nil || cmd.ExpectedRevision < 0 {
		return c, ErrInvalid
	}
	g, err := gates(ctx, tx, actor, true)
	if err != nil {
		return c, err
	}
	if err = lock(ctx, tx, "learning-change-operation:"+actor.Device.ID+":"+cmd.OperationID); err != nil {
		return c, err
	}
	hash := digest(struct {
		Space, Goal, Session, ID string
		Command                  Command
	}{learningspace.Scope(ctx), goal, session, id, cmd})
	var stored, oldID string
	err = tx.QueryRow(ctx, `SELECT request_hash,change_id FROM learning_change_operations WHERE device_id=$1 AND operation_id=$2`, actor.Device.ID, cmd.OperationID).Scan(&stored, &oldID)
	if err == nil {
		if stored != hash {
			return c, ErrConflict
		}
		return s.read(ctx, tx, goal, oldID, 0)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return c, err
	}
	if _, err = s.scope(ctx, tx, goal, session, cmd.Action != "cancel" && cmd.Action != "reject"); err != nil {
		return c, err
	}
	if err = lock(ctx, tx, "learning-change:"+id); err != nil {
		return c, err
	}
	newCandidate := false
	if cmd.Action == "propose" {
		if cmd.ExpectedRevision != 0 || cmd.Candidate == nil || cmd.Base == nil || cmd.Immediate || cmd.Hash != "" || cmd.InteractionID != "" {
			return c, ErrInvalid
		}
		if _, e := s.read(ctx, tx, goal, id, 0); e == nil {
			return c, ErrConflict
		} else if !errors.Is(e, ErrNotFound) {
			return c, e
		}
		c = Change{ID: id, SpaceID: learningspace.Scope(ctx), GoalID: goal, SessionID: session, Revision: 1, Generation: g, CreatedAt: time.Now().UTC()}
	} else {
		c, err = s.read(ctx, tx, goal, id, 0)
		if err != nil {
			return c, err
		}
		if c.SessionID != session || c.Generation != g {
			return c, ErrNotFound
		}
		if c.Revision != cmd.ExpectedRevision || c.Hash != cmd.Hash || c.InteractionID != cmd.InteractionID {
			return c, ErrConflict
		}
		if cmd.Immediate {
			return c, ErrInvalid
		}
	}
	snap, err := s.snapshot(ctx, tx, goal, session, g)
	if err != nil {
		return c, err
	}
	switch cmd.Action {
	case "propose", "revise":
		if cmd.Base == nil || cmd.Candidate == nil || digest(*cmd.Base) != digest(snap.Base) {
			return c, ErrConflict
		}
		if cmd.Action == "revise" {
			if c.Status == "applied" || c.Status == "cancelled" || c.Status == "rejected" {
				return c, ErrConflict
			}
			c.Revision++
			c.Reason = ""
		}
		if err = s.candidate(ctx, tx, &c, snap, *cmd.Candidate); err != nil {
			return c, err
		}
		if cmd.Action == "revise" && c.Status == "proposed" && c.Candidate.Kind != "explanation" {
			c.Status, c.Policy = "waiting_approval", "approval"
		}
		newCandidate = true
	case "approve", "apply_now":
		if cmd.Action == "apply_now" && c.Candidate.Kind != "route" {
			return c, ErrInvalid
		}
		if cmd.Candidate != nil || cmd.Base != nil {
			return c, ErrInvalid
		}
		if cmd.Action == "approve" && c.Status != "waiting_approval" || cmd.Action == "apply_now" && c.Status != "queued_for_boundary" && c.Status != "waiting_approval" {
			return c, ErrConflict
		}
		if digest(c.Base) != digest(snap.Base) {
			c.Status, c.Reason = "stale", "基础版本已变化；原差异保留，请重新生成候选。"
			break
		}
		c.Status = "approved"
		if _, err = tx.Exec(ctx, `UPDATE learning_changes SET device_id=$2,token_id=$3 WHERE id=$1`, c.ID, actor.Device.ID, actor.TokenID); err != nil {
			return c, err
		}
		if err = s.save(ctx, tx, c, false); err != nil {
			return c, err
		}
	case "reject", "cancel":
		if c.Status == "applied" {
			return c, ErrConflict
		}
		c.Status = "cancelled"
		if cmd.Action == "reject" {
			c.Status = "rejected"
		}
	case "restore_focus":
		if c.Status != "applied" || c.FrameID == "" || c.Restored {
			return c, ErrConflict
		}
		if err = s.restore(ctx, tx, actor, &c, snap); err != nil {
			return c, err
		}
	case "compensate":
		if c.Status != "applied" || c.Applied == nil {
			return c, ErrConflict
		}
		original := c
		c = Change{ID: uuid.NewSHA1(uuid.MustParse(cmd.OperationID), []byte(original.ID)).String(), SpaceID: original.SpaceID, GoalID: goal, SessionID: session, Revision: 1, Generation: g, CreatedAt: time.Now().UTC(), Compensates: original.ID}
		input := Candidate{Kind: original.Candidate.Kind, Trigger: "user_request", Reason: "撤回变更，生成补偿版本", EvidenceIDs: []string{}, Steps: original.Diff.BeforeSteps}
		switch input.Kind {
		case "goal":
			input.Steps = nil
			input.Goal = &original.Diff.BeforeGoal
		case "explanation":
			input.Steps = nil
			input.Explanation = "恢复变更前的呈现"
		case "route":
			input.ContextID = original.Base.ContextID
		}
		if err = s.candidate(ctx, tx, &c, snap, input); err != nil {
			return c, err
		}
		if input.Kind == "explanation" {
			c.Diff.AfterContent = original.Diff.BeforeContent
		}
		c.Policy = "approval"
		if c.Status != "needs_sources" {
			c.Status = "waiting_approval"
		}
		c.Impact = "补偿仅产生新版本，后续答案、Evidence 和历史不会撤销。"
		if len(snap.Evidence) > 0 {
			c.Impact += "当前已有正式学习证据，请核对后续影响。"
		}
		if original.Applied.RouteRevisionID != snap.Base.RouteRevisionID || input.Kind == "explanation" && (original.Applied.ArtifactID != snap.Base.ArtifactID || original.Applied.ArtifactVersion != snap.Base.ArtifactVersion) || input.Kind == "goal" && original.Applied.GoalVersion != snap.Base.GoalVersion {
			c.Status, c.Reason = "stale", "之后已有其他变更，不能覆盖它；请以当前版本重新提出调整。"
		}
		c.Hash = candidateHash(c)
		newCandidate = true
	default:
		return c, ErrInvalid
	}
	if cmd.Action == "propose" || cmd.Action == "compensate" {
		_, err = tx.Exec(ctx, `INSERT INTO learning_changes(id,space_id,goal_id,session_id,device_id,token_id,privacy_generation,revision,status) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, c.ID, c.SpaceID, goal, session, actor.Device.ID, actor.TokenID, g, c.Revision, c.Status)
		if err != nil {
			return c, err
		}
	}
	if newCandidate {
		if err = s.save(ctx, tx, c, true); err != nil {
			return c, err
		}
	}
	if c.Status == "proposed" || c.Status == "approved" {
		if c.Candidate.Kind == "route" && !safe(snap.Session.State) && cmd.Action != "apply_now" {
			c.Status = "queued_for_boundary"
			c.Reason = "本题处理后自动应用；原题与草稿保留。"
		} else if err = s.apply(ctx, tx, actor, &c, snap, cmd.Action == "apply_now"); err != nil {
			return c, err
		}
	}
	if err = s.save(ctx, tx, c, false); err != nil {
		return c, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO learning_change_operations(device_id,operation_id,change_id,request_hash) VALUES($1,$2,$3,$4)`, actor.Device.ID, cmd.OperationID, c.ID, hash)
	return c, err
}
func (s *Service) Change(ctx context.Context, actor identity.Credential, goal, session, id string, cmd Command) (Change, error) {
	tx, _, err := s.begin(ctx, actor, true)
	if err != nil {
		return Change{}, err
	}
	defer tx.Rollback(context.Background())
	c, err := s.ChangeTx(ctx, tx, actor, goal, session, id, cmd)
	if err != nil {
		return c, err
	}
	return c, tx.Commit(ctx)
}
