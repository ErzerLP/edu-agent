package agentcontext

import (
	"context"
	"errors"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

type Model interface {
	Complete(context.Context, modelclient.Request) (modelclient.Response, error)
}

// GuardedModel 同时覆盖前台和压缩模型；隐私清除后不能再次发送旧正文。
type GuardedModel struct {
	Base   Model
	Client *Client
}

// 设置创建/恢复时已验证的代次，封闭恢复与首次模型请求之间的清除窗口。
func (c *Client) SetPrivacyBaseline(learner, memory int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.privacySet, c.learner, c.memory = true, learner, memory
}

func (c *Client) Check(ctx context.Context) error {
	page, err := c.ExportMemory(ctx, "", 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		var remote *api.APIError
		if errors.As(err, &remote) && (remote.Code == "content_redacted" || remote.Code == "privacy_clear_in_progress") {
			return err
		}
		// 旧默认区新聊天允许离线本地操作；恢复正文仍由 controller 的隐私隔离处理。
		if !c.privacySet && c.Binding.SpaceID == api.DefaultLearningSpaceID && c.Binding.GoalID == "" {
			return nil
		}
		return err
	}
	l, m := page.ReadGeneration.LearnerGeneration, page.ReadGeneration.MemoryGeneration
	if c.privacySet && (c.learner != l || c.memory != m) {
		return &api.APIError{Code: "content_redacted", Status: 503}
	}
	c.privacySet, c.learner, c.memory = true, l, m
	_, _, err = c.Validate(ctx)
	return err
}

func (m *GuardedModel) Complete(ctx context.Context, r modelclient.Request) (modelclient.Response, error) {
	if err := m.Client.Check(ctx); err != nil {
		return modelclient.Response{}, err
	}
	result, err := m.Base.Complete(ctx, r)
	if err != nil {
		return result, err
	}
	if err := m.Client.Check(ctx); err != nil {
		return modelclient.Response{}, err
	}
	return result, nil
}

func (m *GuardedModel) Stream(ctx context.Context, r modelclient.Request, emit func(modelclient.StreamEvent) error) (modelclient.Response, error) {
	stream, ok := m.Base.(interface {
		Stream(context.Context, modelclient.Request, func(modelclient.StreamEvent) error) (modelclient.Response, error)
	})
	if !ok {
		return m.Complete(ctx, r)
	}
	if err := m.Client.Check(ctx); err != nil {
		return modelclient.Response{}, err
	}
	result, err := stream.Stream(ctx, r, emit)
	if err != nil {
		return result, err
	}
	if err := m.Client.Check(ctx); err != nil {
		return modelclient.Response{}, err
	}
	return result, nil
}
