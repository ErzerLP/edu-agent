package learningchange

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/integrations/learningknowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgepostgres "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningpostgres "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	pool      *pgxpool.Pool
	learning  *learningpostgres.Store
	knowledge *knowledgepostgres.Store
	content   *learningcontent.Store
	aead      cipher.AEAD
}

func New(pool *pgxpool.Pool, l *learningpostgres.Store, k *knowledgepostgres.Store, c *learningcontent.Store, key []byte) (*Service, error) {
	s := &Service{pool: pool, learning: l, knowledge: k, content: c}
	if len(key) == 0 {
		return s, nil
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	s.aead, err = cipher.NewGCM(b)
	return s, err
}
func (s *Service) Available() bool {
	return s != nil && s.aead != nil && s.learning != nil && s.knowledge != nil && s.content.Available()
}
func (s *Service) seal(c Change) ([]byte, error) {
	raw, err := json.Marshal(c)
	if err != nil || len(raw) > 256<<10 {
		return nil, ErrInvalid
	}
	n := make([]byte, s.aead.NonceSize())
	if _, err = rand.Read(n); err != nil {
		return nil, err
	}
	return s.aead.Seal(n, n, raw, []byte(c.ID)), nil
}
func (s *Service) open(id string, raw []byte) (Change, error) {
	var c Change
	if len(raw) < s.aead.NonceSize() {
		return c, ErrNotFound
	}
	n := s.aead.NonceSize()
	plain, err := s.aead.Open(nil, raw[:n], raw[n:], []byte(id))
	if err != nil {
		return c, ErrUnavailable
	}
	err = json.Unmarshal(plain, &c)
	return c, err
}
func gates(ctx context.Context, tx pgx.Tx, actor identity.Credential, write bool) (int64, error) {
	mode, scope := "read", "learning:read"
	if write {
		mode, scope = "write", "learning:write"
	}
	var generation, other int64
	for _, owner := range []string{"identity", "learning", "knowledge", "tutoring"} {
		m := "read"
		if owner == "learning" {
			m = mode
		}
		if err := tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate($1,$2,NULL)`, owner, m).Scan(&other); err != nil {
			return 0, err
		}
		if owner == "learning" {
			generation = other
		}
	}
	var active bool
	err := tx.QueryRow(ctx, `SELECT d.revoked_at IS NULL AND t.revoked_at IS NULL AND $3=ANY(t.scopes) FROM devices d JOIN device_tokens t ON t.device_id=d.id WHERE d.id=$1 AND t.id=$2 FOR SHARE OF d,t`, actor.Device.ID, actor.TokenID, scope).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !active {
		return 0, ErrForbidden
	}
	return generation, err
}
func (s *Service) begin(ctx context.Context, actor identity.Credential, write bool) (pgx.Tx, int64, error) {
	if !s.Available() {
		return nil, 0, ErrUnavailable
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, 0, err
	}
	g, err := gates(ctx, tx, actor, write)
	if err != nil {
		tx.Rollback(context.Background())
		return nil, 0, err
	}
	return tx, g, nil
}
func lock(ctx context.Context, tx pgx.Tx, key string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key)
	return err
}

// 与目标、教学命令使用相同锁顺序，发布期间不允许生命周期或所属关系改变。
func (s *Service) scope(ctx context.Context, tx pgx.Tx, goal, session string, write bool) (learning.GoalRevision, error) {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM learning_spaces WHERE id=$1 FOR SHARE`, learningspace.Scope(ctx)).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return learning.GoalRevision{}, ErrNotFound
	}
	if err != nil {
		return learning.GoalRevision{}, err
	}
	if write && status != "active" {
		return learning.GoalRevision{}, ErrInactive
	}
	if err = lock(ctx, tx, "learning-aggregate:goal:"+goal); err != nil {
		return learning.GoalRevision{}, err
	}
	if session != "" {
		if err = lock(ctx, tx, "learning-aggregate:session:"+session); err != nil {
			return learning.GoalRevision{}, err
		}
	}
	g, err := s.learning.WithinTx(tx).GetGoal(ctx, goal)
	if err != nil {
		return g, err
	}
	status = g.GoalManagement().Status
	if write && status != "active" && status != "draft" {
		return g, ErrInactive
	}
	return g, nil
}

type treeTx struct {
	owner *knowledgepostgres.Store
	tx    pgx.Tx
}

func (t treeTx) Tree(ctx context.Context, id string) (knowledge.TreeResult, error) {
	return t.owner.TreeTx(ctx, t.tx, id)
}
func (s *Service) snapshot(ctx context.Context, tx pgx.Tx, goal, session string, generation int64) (Snapshot, error) {
	r := Snapshot{Steps: []Step{}, Sources: []learning.KnowledgeReference{}, Evidence: []learning.AcceptedEvidence{}}
	l := s.learning.WithinTx(tx)
	var err error
	r.Goal, err = l.GetGoal(ctx, goal)
	if err != nil {
		return r, err
	}
	if err = l.ValidateSessionScope(ctx, session); err != nil {
		return r, err
	}
	a, err := l.LoadSessionAuthority(ctx, session)
	if err != nil {
		return r, err
	}
	r.Session = a.Session
	bound, err := l.LoadGoalRevision(ctx, r.Session.Context.GoalRevisionID)
	if err != nil {
		return r, err
	}
	if bound.GoalID != goal {
		return r, ErrNotFound
	}
	r.Base = Base{GoalVersion: r.Goal.Revision, GoalRevisionID: r.Goal.ID, SessionVersion: r.Session.AggregateVer, RouteRevisionID: r.Session.Context.RouteRevisionID}
	contextID, err := l.ReadSessionContextTx(ctx, tx, session)
	if err != nil {
		return r, err
	}
	if contextID != nil {
		r.Base.ContextID = *contextID
	}
	if r.Base.RouteRevisionID != "" {
		route, e := l.LoadRouteRevision(ctx, r.Base.RouteRevisionID)
		if e != nil {
			return r, e
		}
		for _, st := range route.Steps {
			r.Steps = append(r.Steps, Step{NodeRevisionID: st.NodeRevisionID, Name: st.TeachingIntent, Criterion: st.CompletionCondition, Prompt: st.TeachingIntent + "\n" + st.CompletionCondition, Difficulty: 1, Prerequisites: []int{}})
		}
		if route.SourceProposalID != "" {
			proposal, e := l.LoadProposal(ctx, route.SourceProposalID)
			if e != nil {
				return r, e
			}
			if proposal.ModelID == "reviewed-learning-change" {
				var origin struct {
					ID       string `json:"change_id"`
					Revision int64  `json:"revision"`
					Hash     string `json:"hash"`
				}
				if e = json.Unmarshal(proposal.FrozenRequest.Input, &origin); e != nil {
					return r, e
				}
				candidate, e := s.read(ctx, tx, goal, origin.ID, origin.Revision)
				if e != nil {
					return r, e
				}
				if candidate.Hash != origin.Hash {
					return r, ErrConflict
				}
				// 从正式路线的候选来源恢复完整建议，不把难度、前置和题干重造为默认值。
				r.Steps = candidate.Diff.AfterSteps
			}
		}
	}
	if r.Session.Context.ActivityID != nil {
		r.Base.ActivityID = *r.Session.Context.ActivityID
		a, e := l.LoadActivity(ctx, r.Base.ActivityID)
		if e != nil {
			return r, e
		}
		content, e := s.content.GetTx(ctx, tx, learningcontent.ArtifactID(a), generation)
		if e != nil && !errors.Is(e, learningcontent.ErrNotFound) {
			return r, e
		}
		if e == nil {
			r.Content = &content
			r.Base.ArtifactID = content.ArtifactID
			r.Base.ArtifactVersion = content.Version
		}
		for i, st := range r.Steps {
			if st.NodeRevisionID == r.Session.Context.FocusNodeRevisionID {
				r.Steps[i].Prompt = a.Prompt
				r.Steps[i].Difficulty = a.Difficulty
			}
		}
	}
	scope := r.Session.Context.KnowledgeRevisionID
	refs, err := s.knowledge.EffectiveReferenceTx(ctx, tx, goal, session)
	if err != nil {
		return r, err
	}
	if refs.Version > 0 {
		// 标准变化不能默默回退到旧的更宽范围；发布时要求重新确认目标版本。
		scope, r.AvailableContextID = refs.ScopeSnapshotID, refs.ContextID
	}
	if scope != "" {
		tree, e := s.knowledge.TreeTx(ctx, tx, scope)
		if e != nil {
			return r, e
		}
		resolver := learningknowledge.New(treeTx{s.knowledge, tx})
		for _, doc := range tree.Revision.Documents {
			for _, node := range doc.Revision.Nodes {
				if len(r.Sources) >= 100 {
					break
				}
				if node.SectionRange.End <= node.SectionRange.Start {
					continue
				}
				ref, e := resolver.Resolve(ctx, scope, node.ID)
				if e != nil {
					return r, e
				}
				r.Sources = append(r.Sources, ref)
			}
		}
	}
	for _, st := range r.Steps {
		evidence, e := l.LoadValidEvidence(ctx, st.NodeRevisionID)
		if e != nil {
			return r, e
		}
		for _, v := range evidence {
			g, e := l.LoadGoalRevision(ctx, v.GoalRevisionID)
			if e != nil {
				return r, e
			}
			if g.GoalID == goal && len(r.Evidence) < 100 {
				r.Evidence = append(r.Evidence, v)
			}
		}
	}
	r.Mode, err = mode(ctx, tx, goal)
	return r, err
}
func (s *Service) Snapshot(ctx context.Context, actor identity.Credential, goal, session string) (Snapshot, error) {
	if uuid.Validate(goal) != nil || uuid.Validate(session) != nil {
		return Snapshot{}, ErrInvalid
	}
	tx, g, err := s.begin(ctx, actor, false)
	if err != nil {
		return Snapshot{}, err
	}
	defer tx.Rollback(context.Background())
	if _, err = s.scope(ctx, tx, goal, session, false); err != nil {
		return Snapshot{}, err
	}
	r, err := s.snapshot(ctx, tx, goal, session, g)
	if err != nil {
		return r, err
	}
	return r, tx.Commit(ctx)
}
func mode(ctx context.Context, tx pgx.Tx, goal string) (Mode, error) {
	r := Mode{Mode: "adaptive"}
	err := tx.QueryRow(ctx, `SELECT mode,version FROM learning_adaptive_modes WHERE goal_id=$1 AND space_id=$2`, goal, learningspace.Scope(ctx)).Scan(&r.Mode, &r.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return r, err
}
func (s *Service) Mode(ctx context.Context, actor identity.Credential, goal string, next *Mode) (Mode, error) {
	if uuid.Validate(goal) != nil || next != nil && (next.Version < 0 || next.Mode != "adaptive" && next.Mode != "cautious") {
		return Mode{}, ErrInvalid
	}
	tx, _, err := s.begin(ctx, actor, next != nil)
	if err != nil {
		return Mode{}, err
	}
	defer tx.Rollback(context.Background())
	if _, err = s.scope(ctx, tx, goal, "", next != nil); err != nil {
		return Mode{}, err
	}
	r, err := mode(ctx, tx, goal)
	if err != nil {
		return r, err
	}
	if next != nil {
		if next.Version != r.Version {
			return r, ErrConflict
		}
		r = Mode{Mode: next.Mode, Version: r.Version + 1}
		_, err = tx.Exec(ctx, `INSERT INTO learning_adaptive_modes(goal_id,space_id,version,mode) VALUES($1,$2,$3,$4) ON CONFLICT(goal_id) DO UPDATE SET version=$3,mode=$4`, goal, learningspace.Scope(ctx), r.Version, r.Mode)
		if err != nil {
			return r, err
		}
	}
	return r, tx.Commit(ctx)
}
func (s *Service) read(ctx context.Context, tx pgx.Tx, goal, id string, revision int64) (Change, error) {
	var raw []byte
	var err error
	if revision == 0 {
		err = tx.QueryRow(ctx, `SELECT ciphertext FROM learning_changes WHERE id=$1 AND goal_id=$2 AND space_id=$3`, id, goal, learningspace.Scope(ctx)).Scan(&raw)
	} else {
		err = tx.QueryRow(ctx, `SELECT r.ciphertext FROM learning_change_revisions r JOIN learning_changes c ON c.id=r.change_id WHERE c.id=$1 AND c.goal_id=$2 AND c.space_id=$3 AND r.revision=$4`, id, goal, learningspace.Scope(ctx), revision).Scan(&raw)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Change{}, ErrNotFound
	}
	if err != nil {
		return Change{}, err
	}
	return s.open(id, raw)
}
func (s *Service) Read(ctx context.Context, actor identity.Credential, goal, id string, revision int64) (Change, error) {
	if uuid.Validate(goal) != nil || uuid.Validate(id) != nil || revision < 0 {
		return Change{}, ErrInvalid
	}
	tx, _, err := s.begin(ctx, actor, false)
	if err != nil {
		return Change{}, err
	}
	defer tx.Rollback(context.Background())
	if _, err = s.scope(ctx, tx, goal, "", false); err != nil {
		return Change{}, err
	}
	r, err := s.read(ctx, tx, goal, id, revision)
	if err != nil {
		return r, err
	}
	return r, tx.Commit(ctx)
}
func (s *Service) List(ctx context.Context, actor identity.Credential, goal string) ([]Change, error) {
	if uuid.Validate(goal) != nil {
		return nil, ErrInvalid
	}
	tx, _, err := s.begin(ctx, actor, false)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	if _, err = s.scope(ctx, tx, goal, "", false); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id,ciphertext FROM learning_changes WHERE goal_id=$1 AND space_id=$2 AND ciphertext IS NOT NULL ORDER BY created_at DESC,id DESC LIMIT 100`, goal, learningspace.Scope(ctx))
	if err != nil {
		return nil, err
	}
	items := []Change{}
	for rows.Next() {
		var id string
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		c, e := s.open(id, raw)
		if e != nil {
			rows.Close()
			return nil, e
		}
		items = append(items, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return items, tx.Commit(ctx)
}
func (s *Service) save(ctx context.Context, tx pgx.Tx, c Change, candidate bool) error {
	raw, err := s.seal(c)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE learning_changes SET revision=$2,status=$3,ciphertext=$4,updated_at=clock_timestamp() WHERE id=$1`, c.ID, c.Revision, c.Status, raw)
	if err != nil {
		return err
	}
	if candidate {
		if _, err = tx.Exec(ctx, `INSERT INTO learning_change_revisions(change_id,revision,ciphertext) VALUES($1,$2,$3)`, c.ID, c.Revision, raw); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO learning_change_events(change_id,revision,status) VALUES($1,$2,$3)`, c.ID, c.Revision, c.Status)
	return err
}
func (s *Service) String() string { return fmt.Sprintf("learningchange(available=%t)", s.Available()) }
