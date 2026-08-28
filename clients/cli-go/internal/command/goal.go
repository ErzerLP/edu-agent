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
		return commandError("usage", "goal requires set", "run edu-agent goal set <text>", ExitInput)
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
	current, active, err := a.currentSession(ctx, online.client)
	if err != nil {
		return err
	}
	if active {
		if current.WorkItem == nil || !allowed(current.WorkItem.AllowedActions, "switch_goal") {
			return commandError("invalid_state", "the active session cannot switch goal in its current state", "resolve the current assessment or use a displayed allowed action", ExitConflict)
		}
		_, _ = fmt.Fprintf(a.Out, "Current: state=%s session=%s\n", safeText(current.Session.State), safeText(current.Session.SessionID))
		confirmed, confirmErr := a.Terminal.Confirm("Switching goal may invalidate the active focus. Continue?")
		if confirmErr != nil {
			return commandError("confirmation_failed", "goal switch confirmation could not be read", "retry in an interactive terminal", ExitInput)
		}
		if !confirmed {
			return commandError("goal_switch_declined", "the active session was not changed", "continue learning or run goal set again", ExitInput)
		}
	}
	goal, err := a.createGoal(ctx, online.client, text)
	if err != nil {
		return err
	}
	if !active {
		sessionID, idErr := a.operationID()
		if idErr != nil {
			return idErr
		}
		operationID, idErr := a.operationID()
		if idErr != nil {
			return idErr
		}
		_, createErr := online.client.CreateSession(ctx, api.TutoringSessionRequest{
			OperationID: operationID, PayloadSchemaVersion: 1, AggregateType: "session",
			AggregateID: sessionID, ExpectedVersion: 0, GoalRevisionID: goal.GoalRevisionID,
		})
		if createErr != nil {
			return mapAPIError(createErr)
		}
		fresh, fetchErr := online.client.CurrentSession(ctx)
		if fetchErr != nil {
			return mapAPIError(fetchErr)
		}
		_, err = fmt.Fprintf(a.Out, "Goal: %s\nSession: %s\nState: %s\n", safeText(goal.GoalRevisionID), safeText(fresh.Session.SessionID), safeText(fresh.Session.State))
		return err
	}
	operationID, err := a.operationID()
	if err != nil {
		return err
	}
	fresh, conflict, err := a.applyAndRefetch(ctx, online.client, current, api.ActionSwitchGoalRequest{
		SessionOperation: sessionOperation(current, operationID), Action: "switch_goal", GoalRevisionID: goal.GoalRevisionID,
	})
	if err != nil {
		return err
	}
	if conflict {
		return commandError("version_conflict", "the goal was created but the session changed before it could switch", "inspect the refreshed session before choosing whether to switch again", ExitConflict)
	}
	_, err = fmt.Fprintf(a.Out, "Goal: %s\nSession: %s\nState: %s\n", safeText(goal.GoalRevisionID), safeText(fresh.Session.SessionID), safeText(fresh.Session.State))
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
