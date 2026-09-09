package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

var errSelectTeachingSession = errors.New("选择教学会话")

type teachingSessionClient interface {
	Sessions(context.Context, string, string, string, int) (api.SessionPage, error)
}

func (a *App) teachingSpace() string {
	if a.learningSpace != "" {
		return a.learningSpace
	}
	return api.DefaultLearningSpaceID
}
func (a *App) teachingSpaceLabel() string {
	if a.learningSpaceName != "" {
		return a.learningSpaceName
	}
	if a.teachingSpace() == api.DefaultLearningSpaceID {
		return "默认学习区"
	}
	return a.teachingSpace()
}
func (a *App) selectTeachingSession(id string) {
	if a.learningSessions == nil {
		a.learningSessions = map[string]string{}
	}
	a.learningSessions[a.teachingSpace()] = id
}

func (a *App) startGoalSession(ctx context.Context, client APIClient, goalID string) (api.SessionView, error) {
	goals, ok := client.(goalClient)
	if !ok {
		return api.SessionView{}, commandError("goal_management_unsupported", "客户端尚不支持目标管理", "升级客户端", ExitUnavailable)
	}
	goal, err := goals.Goal(ctx, goalID)
	if err != nil {
		return api.SessionView{}, mapAPIError(err)
	}
	sessionID, err := a.operationID()
	if err != nil {
		return api.SessionView{}, err
	}
	operationID, err := a.operationID()
	if err != nil {
		return api.SessionView{}, err
	}
	_, err = client.CreateSession(ctx, api.TutoringSessionRequest{OperationID: operationID, PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: sessionID, ExpectedVersion: 0, GoalRevisionID: goal.GoalRevisionID})
	if err != nil {
		return api.SessionView{}, mapAPIError(err)
	}
	return refetchSession(ctx, client, sessionID)
}

func (a *App) pickTeachingSession(ctx context.Context, client APIClient, goalID string) (api.SessionView, error) {
	sessions, ok := client.(teachingSessionClient)
	if !ok {
		return api.SessionView{}, commandError("session_selection_unavailable", "客户端尚不支持教学会话选择", "升级客户端", ExitUnavailable)
	}
	if goalID == "" {
		goals, ok := client.(goalClient)
		if !ok {
			return api.SessionView{}, commandError("goal_management_unsupported", "目标列表不可用", "升级客户端", ExitUnavailable)
		}
		cursor := ""
		for {
			page, err := goals.Goals(ctx, "", "", cursor, 20)
			if err != nil {
				return api.SessionView{}, mapAPIError(err)
			}
			_, _ = fmt.Fprintf(a.Out, "学习区：%s · 选择目标\n", safeText(a.teachingSpaceLabel()))
			for i, g := range page.Items {
				m := g.GoalManagement()
				_, _ = fmt.Fprintf(a.Out, "%d. %s · %s\n", i+1, safeText(m.Details.Name), goalStatus(m.Status))
			}
			line, err := a.Terminal.ReadLine("目标编号；n 下一页；q 返回：")
			if err != nil {
				return api.SessionView{}, err
			}
			if line == "q" {
				return api.SessionView{}, nil
			}
			if line == "n" && page.NextCursor != "" {
				cursor = page.NextCursor
				continue
			}
			n, err := strconv.Atoi(strings.TrimSpace(line))
			if err != nil || n < 1 || n > len(page.Items) {
				continue
			}
			goalID = page.Items[n-1].GoalID
			break
		}
	}
	cursor := ""
	for {
		page, err := sessions.Sessions(ctx, goalID, "", cursor, 20)
		if err != nil {
			return api.SessionView{}, mapAPIError(err)
		}
		for i, item := range page.Items {
			_, _ = fmt.Fprintf(a.Out, "%d. %s · %s · %s · 可继续=%t\n", i+1, safeText(item.Name), safeText(item.State), safeText(item.Position), item.Resumable)
		}
		line, err := a.Terminal.ReadLine("会话编号；s 开始新会话；n 下一页；q 返回：")
		if err != nil {
			return api.SessionView{}, err
		}
		if line == "q" {
			return api.SessionView{}, nil
		}
		if line == "s" {
			return a.startGoalSession(ctx, client, goalID)
		}
		if line == "n" && page.NextCursor != "" {
			cursor = page.NextCursor
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || n < 1 || n > len(page.Items) {
			continue
		}
		item := page.Items[n-1]
		view, err := refetchSession(ctx, client, item.SessionID)
		if err != nil {
			return view, mapAPIError(err)
		}
		if !item.Resumable {
			_, _ = fmt.Fprintf(a.Out, "历史会话：%s · %s；目标状态：%s\n", safeText(item.Name), safeText(item.State), goalStatus(item.GoalStatus))
			if err := json.NewEncoder(a.Out).Encode(view); err != nil {
				return view, err
			}
			continue
		}
		return view, nil
	}
}
