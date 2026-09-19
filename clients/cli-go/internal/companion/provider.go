// Package companion 将既有 CLI provider 适配为显式授权的临时本地通道。
package companion

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
	core "github.com/edu-agent/edu-agent/packages/agentcore"
	wire "github.com/edu-agent/edu-agent/packages/agentcore/companion"
)

type Provider struct {
	device   wire.Device
	files    *workspace.Workspace
	tasks    *localexec.Manager
	grant    *wire.Grant
	receipts map[string]wire.Receipt
	plans    map[string]*workspace.PreparedMutation
	taskRuns map[string]string
}

func NewProvider(device wire.Device) (*Provider, error) {
	w, err := workspace.Open(device.Workspace)
	if err != nil {
		return nil, err
	}
	return &Provider{device: device, files: w, tasks: localexec.New(localexec.Options{}), receipts: map[string]wire.Receipt{}, plans: map[string]*workspace.PreparedMutation{}, taskRuns: map[string]string{}}, nil
}

// Close 的顺序保证先结算受管任务，再关闭工作区。无磁盘会话或输出存储。
func (p *Provider) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := p.tasks.Close(ctx)
	_ = p.files.Close()
	p.plans = nil
	return err
}

func (p *Provider) Execute(ctx context.Context, g wire.Grant, op wire.Operation) wire.Receipt {
	r := wire.Receipt{ID: op.ID, Device: p.device.ID, Conversation: g.Conversation, Run: op.Run, State: "completed"}
	if p.grant == nil {
		copy := g
		p.grant = &copy
	}
	if *p.grant != g || !wire.Allowed(g, op.Tool, false) {
		r.Error = "companion_not_authorized"
		return r
	}
	if old, ok := p.receipts[op.ID]; ok {
		return old
	}
	if len(p.receipts) >= 1024 {
		r.Error = "companion_capacity"
		return r
	}
	// 先消费身份，未知结果也不能通过重新调用重放。
	p.receipts[op.ID] = wire.Receipt{ID: r.ID, Device: r.Device, Conversation: r.Conversation, Run: r.Run, State: "unknown"}
	value, err := p.execute(ctx, g.Conversation, op)
	if snapshot, ok := value.(localexec.Snapshot); ok {
		r.Task = snapshot.TaskID
		if op.Tool == "shell" && r.Task != "" {
			p.taskRuns[r.Task] = op.Run
		}
	}
	if op.Tool == "task" {
		if args, e := agentloop.DecodeTask(string(op.Arguments)); e == nil {
			r.Task = args.TaskID
		}
	}
	r.TaskRun = p.taskRuns[r.Task]
	if err != nil {
		r.Error = "invalid_local_operation"
		var le *localexec.Error
		if errors.As(err, &le) {
			r.Error = le.Code
		}
	}
	data, marshalErr := json.Marshal(value)
	if marshalErr != nil || len(data) > wire.MaxPayload/2 {
		r.Error = "result_too_large"
		data = []byte(`{"outcome":"unknown","reason":"结果超过通道预算，请查询原任务或文件"}`)
	}
	r.Value = data
	p.receipts[op.ID] = r
	return r
}

func (p *Provider) execute(ctx context.Context, owner string, op wire.Operation) (any, error) {
	switch op.Tool {
	case "list", "read", "stat":
		return p.files.Execute(ctx, op.Tool, string(op.Arguments)), nil
	case "prepare_write", "prepare_edit":
		if len(p.plans) >= 1 {
			return nil, errors.New("pending_plan")
		}
		plan, result := p.files.PrepareMutation(ctx, strings.TrimPrefix(op.Tool, "prepare_"), string(op.Arguments))
		if plan == nil {
			return result, nil
		}
		preview := plan.Presentation.Preview
		if plan.FullDiff() != "" {
			preview = plan.FullDiff()
		}
		if len(preview) > 48<<10 {
			return nil, errors.New("preview_limit")
		}
		id := wire.Secret()
		p.plans[id] = plan
		return map[string]any{"plan": id, "preview": preview, "path": plan.Presentation.Path, "status": "confirmation_required"}, nil
	case "commit", "discard":
		var args struct {
			Plan string `json:"plan"`
		}
		if core.DecodeArguments(string(op.Arguments), &args) != nil {
			return nil, errors.New("invalid_plan")
		}
		plan := p.plans[args.Plan]
		if plan == nil {
			return nil, errors.New("invalid_plan")
		}
		delete(p.plans, args.Plan)
		if op.Tool == "discard" {
			return map[string]string{"status": "discarded"}, nil
		}
		return p.files.CommitMutation(ctx, plan), nil
	case "shell":
		args, wait, err := agentloop.DecodeShell(string(op.Arguments), p.device.Workspace)
		if err != nil {
			return nil, err
		}
		snapshot, err := p.tasks.Start(ctx, owner, op.ID, args)
		if err == nil && wait > 0 {
			snapshot, err = p.tasks.Wait(ctx, owner, snapshot.TaskID, wait)
		}
		return snapshot, err
	case "task":
		a, err := agentloop.DecodeTask(string(op.Arguments))
		if err != nil {
			return nil, err
		}
		switch a.Action {
		case "list":
			all := p.tasks.List(owner)
			start := int(min(a.Offset, int64(len(all))))
			end := min(start+a.Limit, len(all))
			return map[string]any{"items": all[start:end], "next_offset": end, "more": end < len(all)}, nil
		case "status":
			return p.tasks.Status(owner, a.TaskID)
		case "read":
			page, err := p.tasks.Read(owner, a.TaskID, a.Stream, a.Offset, min(a.Limit, 16<<10))
			return struct {
				localexec.OutputPage
				Text string `json:"text"`
			}{page, string(page.Data)}, err
		case "wait":
			return p.tasks.Wait(ctx, owner, a.TaskID, time.Duration(a.WaitMS)*time.Millisecond)
		case "stop":
			return p.tasks.Stop(ctx, owner, a.TaskID)
		case "input":
			return p.tasks.WriteInput(ctx, owner, a.TaskID, []byte(a.Content))
		case "close_input":
			return map[string]string{"action": "close_input"}, p.tasks.CloseInput(owner, a.TaskID)
		case "interrupt":
			return p.tasks.Interrupt(ctx, owner, a.TaskID)
		case "eof":
			return p.tasks.SendEOF(ctx, owner, a.TaskID)
		case "resize":
			return p.tasks.Resize(owner, a.TaskID, a.Rows, a.Cols)
		case "search":
			return p.tasks.Search(ctx, owner, a.TaskID, a.Stream, a.Needle, a.Offset, a.Limit)
		}
	}
	return nil, errors.New("unknown_operation")
}
