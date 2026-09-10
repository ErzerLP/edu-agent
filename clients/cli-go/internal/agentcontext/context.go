// Package agentcontext 将 Agent 的业务身份绑定到正式服务客户端。
package agentcontext

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

type Binding struct {
	SpaceID   string `json:"space_id"`
	GoalID    string `json:"goal_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (b Binding) Normalize() Binding {
	if b.SpaceID == "" {
		b.SpaceID = api.DefaultLearningSpaceID
	}
	return b
}

func (b Binding) Valid() bool {
	b = b.Normalize()
	for _, id := range []string{b.SpaceID, b.GoalID, b.SessionID} {
		if id != "" && (!uuid.MatchString(id) || id == "00000000-0000-0000-0000-000000000000") {
			return false
		}
	}
	return b.SessionID == "" || b.GoalID != ""
}

func (b Binding) Label() string {
	b = b.Normalize()
	s := "学习区 " + b.SpaceID
	if b.SpaceID == api.DefaultLearningSpaceID {
		s = "默认学习区"
	}
	if b.GoalID != "" {
		s += " · 目标 " + b.GoalID
	} else {
		s += " · 区内交流（未选目标）"
	}
	if b.SessionID != "" {
		s += " · 教学 " + b.SessionID
	}
	return s
}

func (b Binding) ShortLabel() string {
	b = b.Normalize()
	short := func(id string) string {
		if len(id) > 8 {
			return "…" + id[len(id)-8:]
		}
		return id
	}
	label := "区 " + short(b.SpaceID)
	if b.SpaceID == api.DefaultLearningSpaceID {
		label = "默认区"
	}
	if b.GoalID == "" {
		return label + " · 未选目标"
	}
	label += " · 目标 " + short(b.GoalID)
	if b.SessionID != "" {
		label += " · 教学 " + short(b.SessionID)
	}
	return label
}

type Client struct {
	*api.Client
	Binding         Binding
	global          *api.Client
	mu              sync.Mutex
	privacySet      bool
	learner, memory int64
}

func New(client *api.Client, binding Binding) *Client {
	binding = binding.Normalize()
	return &Client{Client: client.WithLearningSpace(binding.SpaceID), Binding: binding, global: client.WithLearningSpace(api.DefaultLearningSpaceID)}
}

func (c *Client) Rebind(b Binding) *Client { return New(c.Client, b) }

func (c *Client) AgentBinding() string {
	b := c.Binding
	return b.SpaceID + "/" + b.GoalID + "/" + b.SessionID
}

// Validate 每次读取正式归属；不缓存服务端生命周期或模型提供的关系。
func (c *Client) Validate(ctx context.Context) (api.LearningSpace, *api.GoalRevision, error) {
	if !c.Binding.Valid() {
		return api.LearningSpace{}, nil, fmt.Errorf("invalid_agent_binding")
	}
	s, err := c.Client.LearningSpace(ctx, c.Binding.SpaceID)
	if err != nil {
		return s, nil, err
	}
	var goal *api.GoalRevision
	if c.Binding.GoalID != "" {
		g, err := c.Client.Goal(ctx, c.Binding.GoalID)
		if err != nil {
			return s, nil, err
		}
		if (Binding{SpaceID: g.SpaceID}).Normalize().SpaceID != c.Binding.SpaceID {
			return s, nil, fmt.Errorf("agent_goal_scope_mismatch")
		}
		goal = &g
	}
	if c.Binding.SessionID != "" {
		if _, err := c.boundSession(ctx); err != nil {
			return s, goal, err
		}
	}
	return s, goal, nil
}

func (c *Client) boundSession(ctx context.Context) (api.SessionSummary, error) {
	cursor := ""
	for {
		page, err := c.Client.Sessions(ctx, c.Binding.GoalID, "", cursor, 100)
		if err != nil {
			return api.SessionSummary{}, err
		}
		for _, item := range page.Items {
			if item.SessionID == c.Binding.SessionID && item.LearningSpaceID == c.Binding.SpaceID && item.GoalID == c.Binding.GoalID {
				return item, nil
			}
		}
		if page.NextCursor == "" {
			return api.SessionSummary{}, fmt.Errorf("agent_session_goal_mismatch")
		}
		cursor = page.NextCursor
	}
}

func (c *Client) CurrentSession(ctx context.Context) (api.SessionView, error) {
	if c.Binding.SessionID == "" {
		return api.SessionView{}, &api.APIError{Code: "not_found", Status: 404}
	}
	if _, _, err := c.Validate(ctx); err != nil {
		return api.SessionView{}, err
	}
	return c.Client.Session(ctx, c.Binding.SessionID)
}

func (c *Client) RetrieveKnowledge(ctx context.Context, request api.KnowledgeRetrievalRequest) (api.KnowledgeRetrievalResult, error) {
	_, g, err := c.Validate(ctx)
	if err != nil {
		return api.KnowledgeRetrievalResult{}, err
	}
	request.KnowledgeRevisionID = ""
	if g == nil {
		return api.KnowledgeRetrievalResult{}, &api.APIError{Code: "goal_selection_required", Status: 409}
	}
	request.ScopeSnapshotID = g.GoalManagement().Details.ScopeSnapshotID
	if c.Binding.SessionID != "" {
		// 教学会话冻结旧目标版本，不能随目标编辑静默换用新资料。
		session, err := c.boundSession(ctx)
		if err != nil {
			return api.KnowledgeRetrievalResult{}, err
		}
		request.ScopeSnapshotID = session.ScopeSnapshotID
	}
	if request.ScopeSnapshotID == "" {
		return api.KnowledgeRetrievalResult{}, &api.APIError{Code: "knowledge_scope_required", Status: 409}
	}
	return c.Client.RetrieveKnowledge(ctx, request)
}

func (c *Client) query(q api.ProgressQuery) api.ProgressQuery {
	q.Global, q.LearningSpaceID, q.GoalID = false, c.Binding.SpaceID, c.Binding.GoalID
	if c.Binding.GoalID != "" {
		q.Status = "all"
	}
	return q
}
func (c *Client) Progress(ctx context.Context, q api.ProgressQuery) (api.ProgressPage, error) {
	if _, _, err := c.Validate(ctx); err != nil {
		return api.ProgressPage{}, err
	}
	return c.Client.Progress(ctx, c.query(q))
}
func (c *Client) ScopedReviews(ctx context.Context, q api.ProgressQuery, due *time.Time) (api.ReviewsPage, error) {
	if _, _, err := c.Validate(ctx); err != nil {
		return api.ReviewsPage{}, err
	}
	return c.Client.ScopedReviews(ctx, c.query(q), due)
}

// 长期偏好沿用明确的全局合同，业务学习区不升级为记忆命名空间。
func (c *Client) ExportMemory(ctx context.Context, cursor string, limit int) (api.MemoryExportPage, error) {
	return c.global.ExportMemory(ctx, cursor, limit)
}
func (c *Client) MemoryCandidate(ctx context.Context, id string) (api.MemoryCandidateView, error) {
	return c.global.MemoryCandidate(ctx, id)
}
func (c *Client) CreateMemoryCandidate(ctx context.Context, r api.MemoryCandidateRequest) (api.MemoryOperationResponse, error) {
	return c.global.CreateMemoryCandidate(ctx, r)
}
func (c *Client) DecideMemoryCandidate(ctx context.Context, id string, r api.MemoryCandidateDecisionRequest) (api.MemoryOperationResponse, error) {
	return c.global.DecideMemoryCandidate(ctx, id, r)
}

func (c *Client) Description(ctx context.Context) (string, error) {
	s, g, err := c.Validate(ctx)
	if err != nil {
		return "", err
	}
	parts := []string{"学习区：" + s.Name + "（" + s.Status + "）"}
	if g != nil {
		parts = append(parts, "目标："+g.GoalManagement().Details.Name+"（"+g.GoalManagement().Status+"）")
	} else {
		parts = append(parts, "区内交流，未绑定目标")
	}
	if c.Binding.SessionID == "" {
		parts = append(parts, "未绑定教学会话")
	}
	return strings.Join(parts, " · "), nil
}
