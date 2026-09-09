package command

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

func (a *App) runGoal(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "set" {
		return a.runGoalManagement(ctx, args)
	}
	set := newFlagSet("goal set")
	var flags onlineFlags
	addOnlineFlags(set, &flags)
	if err := set.Parse(args[1:]); err != nil || len(set.Args()) == 0 {
		return commandError("usage", "goal set requires non-empty text", "run edu-agent goal set <text>", ExitInput)
	}
	text := strings.TrimSpace(strings.Join(set.Args(), " "))
	if text == "" || len([]rune(text)) > 4000 || !utf8.ValidString(text) {
		return commandError("invalid_goal", "goal text must be valid UTF-8 with 1 to 4000 characters", "provide a shorter non-empty goal", ExitInput)
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	goal, err := a.createGoal(ctx, online.client, text)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.Out, "Goal: %s\n已保存目标：%s\n", safeText(goal.GoalRevisionID), safeText(goal.Text))
	return err
}

func (a *App) createGoal(ctx context.Context, client APIClient, text string) (api.GoalRevision, error) {
	goalID, err := a.operationID()
	if err != nil {
		return api.GoalRevision{}, err
	}
	operationID, err := a.operationID()
	if err != nil {
		return api.GoalRevision{}, err
	}
	result, err := client.CreateGoal(ctx, api.LearningGoalRequest{
		OperationID: operationID, PayloadSchemaVersion: 1, AggregateType: "goal",
		AggregateID: goalID, ExpectedVersion: 0, Text: text, Source: "go-cli-m1",
	})
	if err != nil {
		return api.GoalRevision{}, mapAPIError(err)
	}
	goal := result.Result
	if goal.GoalID != goalID || goal.Source != "go-cli-m1" {
		return api.GoalRevision{}, commandError("protocol_error", "goal creation returned an inconsistent public result", "check the server version", ExitInternal)
	}
	return goal, nil
}
