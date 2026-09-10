package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentcontext"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/terminal"
)

func (a *App) runAgentWorkflow(ctx context.Context, flow agentloop.LearningWorkflow, in io.Reader, out io.Writer) (result agentloop.WorkflowOutcome) {
	local := *a
	local.learningSpace = flow.Binding.Normalize().SpaceID
	local.Out, local.Err = out, out
	local.Terminal = terminal.New(in, out, out)
	local.teachingInput, local.teachingOutput = in, out
	local.importDrafts = map[string]*importDraft{}
	local.agentWorkflow = true
	result.Status = "returned"
	online, err := local.openOnline(onlineFlags{})
	if err != nil {
		return workflowFailure(err)
	}
	client, ok := online.client.(*api.Client)
	if !ok {
		return agentloop.WorkflowOutcome{Status: "unavailable", Code: "learning_context_unavailable"}
	}
	// 原绑定已归档、删除或清除时，仍允许用户离开失效上下文。
	if flow.Kind == "select_context" {
		fmt.Fprintln(out, "切换保留原聊天绑定并结束当前运行时；本地任务按原会话退出规则清理，不迁移到新区。")
		binding, err := local.chooseAgentContext(ctx, client)
		if err != nil {
			return workflowFailure(err)
		}
		if binding == nil {
			return agentloop.WorkflowOutcome{Status: "cancelled"}
		}
		return agentloop.WorkflowOutcome{Status: "selected", Navigate: binding}
	}
	bound := agentcontext.New(client, flow.Binding)
	space, _, err := bound.Validate(ctx)
	if err != nil {
		return workflowFailure(err)
	}
	local.learningSpaceName = space.Name
	fmt.Fprintln(out, "学习区：", safeText(space.Name), "；以下为正式业务页面，保存和采用按页面确认。")
	if space.Status == "archived" {
		return agentloop.WorkflowOutcome{Status: "unavailable", Code: "learning_space_archived"}
	}
	if flow.Kind == "import" {
		err = local.runImportWizard(ctx, client, "", "")
		if err != nil {
			return workflowFailure(err)
		}
		draft := local.importDrafts[local.learningSpace]
		if draft != nil && draft.result != nil {
			return agentloop.WorkflowOutcome{Status: "published", Data: draft.result}
		}
		return agentloop.WorkflowOutcome{Status: "returned_without_confirmed_publication"}
	}
	goalID := flow.Binding.GoalID
	if goalID == "" {
		g, err := local.chooseAgentGoal(ctx, client, false)
		if err != nil {
			return workflowFailure(err)
		}
		if g == nil {
			return agentloop.WorkflowOutcome{Status: "cancelled"}
		}
		goalID = g.GoalID
	}
	switch flow.Kind {
	case "goal":
		err = local.browseGoalDetail(ctx, client, goalID)
	case "planning":
		err = local.browsePlanning(ctx, client, goalID, "", flow.Binding.SessionID)
	default:
		return agentloop.WorkflowOutcome{Status: "failed", Code: "invalid_workflow"}
	}
	if err != nil {
		return workflowFailure(err)
	}
	goal, err := client.Goal(ctx, goalID)
	if err != nil {
		return workflowFailure(err)
	}
	if flow.Kind == "planning" {
		plans, err := client.PlanningList(ctx, goalID)
		if err != nil {
			return workflowFailure(err)
		}
		result.Data = map[string]any{"goal": goal, "plans": plans}
	} else {
		result.Data = goal
	}
	return result
}

func workflowFailure(err error) agentloop.WorkflowOutcome {
	code := "workflow_failed"
	var remote *api.APIError
	var local *Error
	if errors.As(err, &remote) {
		code = remote.Code
	} else if errors.As(err, &local) {
		code = local.Code
	}
	if errors.Is(err, context.Canceled) {
		code = "cancelled_outcome_unconfirmed"
	}
	return agentloop.WorkflowOutcome{Status: "failed", Code: code}
}

func (a *App) chooseAgentGoal(ctx context.Context, client *api.Client, allowGeneral bool) (*api.GoalRevision, error) {
	cursor := ""
	for {
		page, err := client.Goals(ctx, "", "", cursor, 20)
		if err != nil {
			return nil, err
		}
		for i, g := range page.Items {
			fmt.Fprintf(a.Out, "%d. %s · %s · 版本 %d\n", i+1, safeText(g.GoalManagement().Details.Name), safeText(g.GoalManagement().Status), g.Revision)
		}
		prompt := "目标序号；n 下一页；c 新建目标草稿；q 取消 > "
		if allowGeneral {
			prompt = "目标序号；Enter 区内通用交流；n 下一页；c 新建目标草稿；q 取消 > "
		}
		choice, err := a.Terminal.ReadLine(prompt)
		if err != nil {
			return nil, err
		}
		choice = strings.TrimSpace(choice)
		if choice == "q" {
			return nil, context.Canceled
		}
		if choice == "" && allowGeneral {
			return nil, nil
		}
		if choice == "n" && page.NextCursor != "" {
			cursor = page.NextCursor
			continue
		}
		if choice == "c" {
			text, err := a.Terminal.ReadLine("一句话学习意图 > ")
			if err != nil {
				return nil, err
			}
			goalID, err := a.operationID()
			if err != nil {
				return nil, err
			}
			if err := a.editGoal(ctx, client, api.GoalRevision{GoalID: goalID, Text: text}); err != nil {
				return nil, err
			}
			cursor = ""
			continue
		}
		n, err := strconv.Atoi(choice)
		if err == nil && n > 0 && n <= len(page.Items) {
			return &page.Items[n-1], nil
		}
	}
}

func (a *App) chooseAgentContext(ctx context.Context, client *api.Client) (*agentcontext.Binding, error) {
	cursor := ""
	for {
		page, err := client.LearningSpaces(ctx, "", "", cursor, 20)
		if err != nil {
			return nil, err
		}
		for i, s := range page.Items {
			fmt.Fprintf(a.Out, "%d. %s · %s\n", i+1, safeText(s.Name), safeText(s.Status))
		}
		answer, err := a.Terminal.ReadLine("学习区序号；n 下一页；q 取消。选择后进入该区独立聊天 > ")
		if err != nil {
			return nil, err
		}
		if answer == "q" {
			return nil, nil
		}
		if answer == "n" && page.NextCursor != "" {
			cursor = page.NextCursor
			continue
		}
		n, err := strconv.Atoi(answer)
		if err != nil || n < 1 || n > len(page.Items) {
			continue
		}
		binding := agentcontext.Binding{SpaceID: page.Items[n-1].ID}
		a.learningSpace, a.learningSpaceName = binding.SpaceID, page.Items[n-1].Name
		client = client.WithLearningSpace(binding.SpaceID)
		g, err := a.chooseAgentGoal(ctx, client, true)
		if errors.Is(err, context.Canceled) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if g != nil {
			binding.GoalID = g.GoalID
			cursor = ""
			for {
				page, err := client.Sessions(ctx, g.GoalID, "", cursor, 20)
				if err != nil {
					return nil, err
				}
				for i, s := range page.Items {
					fmt.Fprintf(a.Out, "%d. %s · %s · %s\n", i+1, safeText(s.Name), safeText(s.State), safeText(s.Position))
				}
				choice, err := a.Terminal.ReadLine("可选教学会话序号；Enter 不绑定教学；n 下一页；q 取消。聊天恢复不会重放教学动作 > ")
				if err != nil {
					return nil, err
				}
				if choice == "q" {
					return nil, nil
				}
				if choice == "" {
					break
				}
				if choice == "n" && page.NextCursor != "" {
					cursor = page.NextCursor
					continue
				}
				i, err := strconv.Atoi(choice)
				if err == nil && i > 0 && i <= len(page.Items) {
					binding.SessionID = page.Items[i-1].SessionID
					break
				}
			}
		}
		return &binding, nil
	}
}
