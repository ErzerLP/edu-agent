package command

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

func (a *App) runOfflineAssessments(ctx context.Context, args []string) error {
	set := newFlagSet("offline assessments")
	var flags onlineFlags
	var cursor, status string
	var limit int
	addOnlineFlags(set, &flags)
	set.StringVar(&cursor, "cursor", "", "opaque page cursor")
	set.IntVar(&limit, "limit", defaultPageLimit, "page size")
	set.StringVar(&status, "status", "provisional", "assessment status")
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 || status != "provisional" {
		return commandError("usage", "offline assessments flags are invalid", "use --status provisional, --limit 1..200, and an optional cursor", ExitInput)
	}
	if err := validatePageInput(limit, cursor); err != nil {
		return err
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	page, err := online.client.OfflineAssessments(ctx, cursor, limit, status)
	if err != nil {
		var apiErr *api.APIError
		if cursor != "" && errors.As(err, &apiErr) && apiErr.Code == "stale_cursor" {
			_, _ = fmt.Fprintln(a.Err, "warning[stale_cursor]: offline assessments changed; restarting from the first page")
			page, err = online.client.OfflineAssessments(ctx, "", limit, status)
		}
		if err != nil {
			return mapAPIError(err)
		}
	}
	printProjectionWarning(a.Err, page.Metadata)
	if len(page.Items) == 0 {
		_, err = fmt.Fprintln(a.Out, "No provisional offline assessments.")
		return err
	}
	for _, item := range page.Items {
		_, _ = fmt.Fprintf(a.Out, "Offline assessment: %s attempt=%s disposition=%s version=%s confidence=%d confirmable=%t\n",
			safeText(item.AssessmentID), safeText(item.AttemptID), safeText(item.Disposition), safeText(string(item.DispositionVersion)), item.Confidence, item.Confirmable)
		_, _ = fmt.Fprintf(a.Out, "Allowed decisions: %s\n", safeText(strings.Join(item.AllowedDecisions, ",")))
	}
	if page.NextCursor != "" {
		_, _ = fmt.Fprintf(a.Out, "warning[truncated]: more offline assessments are available next_cursor=%s\n", safeText(page.NextCursor))
	}
	return nil
}

func (a *App) runOfflineAssessment(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return commandError("usage", "offline assessment requires show, confirm, override, or void", "run edu-agent offline assessment show ASSESSMENT_UUID", ExitInput)
	}
	kind := args[0]
	if kind != "show" && kind != "confirm" && kind != "override" && kind != "void" {
		return commandError("usage", "offline assessment action is unknown", "use show, confirm, override, or void", ExitInput)
	}
	set := newFlagSet("offline assessment " + kind)
	var flags onlineFlags
	operationID := ""
	addOnlineFlags(set, &flags)
	if kind != "show" {
		set.StringVar(&operationID, "operation-id", "", "stable decision operation UUID")
	}
	if err := set.Parse(args[1:]); err != nil || len(set.Args()) != 1 {
		return commandError("usage", "offline assessment requires one assessment UUID", "run edu-agent offline assessment "+kind+" ASSESSMENT_UUID", ExitInput)
	}
	assessmentID := set.Args()[0]
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	view, err := online.client.OfflineAssessment(ctx, assessmentID)
	if err != nil {
		return mapAPIError(err)
	}
	printProjectionWarning(a.Err, view.Metadata)
	if kind == "show" {
		printOfflineAssessment(a.Out, view)
		return nil
	}
	if !allowed(view.AllowedDecisions, kind) {
		printOfflineAssessment(a.Out, view)
		return commandError("assessment_decision_not_allowed", "the offline assessment does not allow this decision", "refresh with offline assessment show and use an allowed decision", ExitConflict)
	}
	if operationID == "" {
		operationID, err = a.operationID()
		if err != nil {
			return err
		}
	}
	base := api.OfflineAssessmentDecisionBase{
		OperationID: operationID, PayloadSchemaVersion: 1, AttemptID: view.Attempt.AttemptID,
		ExpectedVersion: view.AggregateVersion, Kind: kind,
		ExpectedDispositionVersion: api.Uint63Decimal(fmt.Sprintf("%d", view.Decision.Version)),
	}
	var request api.OfflineAssessmentDecisionRequest
	switch kind {
	case "confirm":
		request = api.OfflineAssessmentConfirmRequest{OfflineAssessmentDecisionBase: base}
	case "override":
		reason, readErr := a.Terminal.ReadLine("Override reason: ")
		reason = strings.TrimSpace(reason)
		if readErr != nil || reason == "" || !utf8.ValidString(reason) || utf8.RuneCountInString(reason) > api.MaxOfflineAssessmentDecisionReasonRunes {
			return commandError("invalid_assessment_reason", "override requires a reason within the public contract limit", "retry and explain the correction", ExitInput)
		}
		items, collectErr := a.collectOfflineAssessmentOverride(view)
		if collectErr != nil {
			return collectErr
		}
		request = api.OfflineAssessmentOverrideRequest{
			OfflineAssessmentDecisionBase: base, Reason: reason, Items: items,
		}
	case "void":
		reason, readErr := a.Terminal.ReadLine("Void reason: ")
		reason = strings.TrimSpace(reason)
		if readErr != nil || reason == "" || !utf8.ValidString(reason) || utf8.RuneCountInString(reason) > api.MaxOfflineAssessmentDecisionReasonRunes {
			return commandError("invalid_assessment_reason", "void requires a reason within the public contract limit", "retry and explain why the assessment should not count", ExitInput)
		}
		request = api.OfflineAssessmentVoidRequest{OfflineAssessmentDecisionBase: base, Reason: reason}
	}
	receipt, err := online.client.DecideOfflineAssessment(ctx, assessmentID, request)
	if err != nil {
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && (apiErr.Code == "version_conflict" || apiErr.Code == "assessment_disposition_conflict") {
			if fresh, fetchErr := online.client.OfflineAssessment(ctx, assessmentID); fetchErr == nil {
				printOfflineAssessment(a.Out, fresh)
			}
		}
		return mapAPIError(err)
	}
	printOfflineAssessmentDecisionReceipt(a.Out, receipt)
	return nil
}

func (a *App) collectOfflineAssessmentOverride(view api.OfflineAssessmentView) ([]api.OfflineAssessmentOverrideItem, error) {
	currentByRubric := make(map[string]api.AssessmentItem, len(view.Decision.Items))
	for _, current := range view.Decision.Items {
		if _, exists := currentByRubric[current.RubricItemID]; exists {
			return nil, commandError("protocol_error", "the offline assessment decision has duplicate rubric items", "refresh the assessment", ExitInternal)
		}
		currentByRubric[current.RubricItemID] = current
	}
	if len(currentByRubric) != len(view.Assessment.Items) {
		return nil, commandError("protocol_error", "the offline assessment decision does not match the immutable artifact", "refresh the assessment", ExitInternal)
	}
	items := make([]api.OfflineAssessmentOverrideItem, len(view.Assessment.Items))
	for index, source := range view.Assessment.Items {
		current, ok := currentByRubric[source.RubricItemID]
		if !ok || !sameAssessmentSource(source, current) || !utf8.ValidString(source.RubricItemID) ||
			strings.TrimSpace(source.RubricItemID) == "" || utf8.RuneCountInString(source.RubricItemID) > api.MaxOfflineAssessmentRubricItemIDRunes {
			return nil, commandError("protocol_error", "the offline assessment decision changed immutable source fields", "refresh the assessment", ExitInternal)
		}
		_, _ = fmt.Fprintf(a.Out, "Rubric: %s current=%s\n", safeText(source.RubricItemID), safeText(current.Conclusion))
		conclusion, err := a.Terminal.ReadLine(fmt.Sprintf("Conclusion (pass/partial/fail) [%s]: ", safeText(current.Conclusion)))
		if err != nil {
			return nil, commandError("assessment_input_failed", "override conclusion could not be read", "retry the offline assessment override", ExitInput)
		}
		conclusion = strings.TrimSpace(strings.ToLower(conclusion))
		if conclusion == "" {
			conclusion = current.Conclusion
		}
		if conclusion != "pass" && conclusion != "partial" && conclusion != "fail" {
			return nil, commandError("invalid_assessment_conclusion", "override conclusion must be pass, partial, or fail", "retry the offline assessment override", ExitInput)
		}
		candidate, err := a.Terminal.ReadLine("Misconception candidate (blank preserves current): ")
		if err != nil {
			return nil, commandError("assessment_input_failed", "misconception candidate could not be read", "retry the offline assessment override", ExitInput)
		}
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			candidate = current.MisconceptionCandidate
		}
		if !utf8.ValidString(candidate) || utf8.RuneCountInString(candidate) > api.MaxOfflineAssessmentMisconceptionRunes {
			return nil, commandError("invalid_assessment_misconception", "misconception candidate exceeds the public contract limit", "retry the offline assessment override", ExitInput)
		}
		items[index] = api.OfflineAssessmentOverrideItem{
			RubricItemID: source.RubricItemID, Conclusion: conclusion, MisconceptionCandidate: candidate,
		}
	}
	return items, nil
}

func printOfflineAssessment(out interface{ Write([]byte) (int, error) }, view api.OfflineAssessmentView) {
	_, _ = fmt.Fprintf(out, "Offline assessment: %s attempt=%s submission=%s aggregate_version=%s disposition=%s decision_version=%d confidence=%d confirmable=%t\n",
		safeText(view.Assessment.AssessmentID), safeText(view.Attempt.AttemptID), safeText(view.SubmissionID),
		safeText(string(view.AggregateVersion)), safeText(view.Decision.Disposition), view.Decision.Version,
		view.Assessment.Confidence, view.Confirmable)
	for _, item := range view.Decision.Items {
		_, _ = fmt.Fprintf(out, "Rubric: %s conclusion=%s\n", safeText(item.RubricItemID), safeText(item.Conclusion))
	}
	_, _ = fmt.Fprintf(out, "Allowed decisions: %s\n", safeText(strings.Join(view.AllowedDecisions, ",")))
}

func printOfflineAssessmentDecisionReceipt(out interface{ Write([]byte) (int, error) }, receipt api.OfflineAssessmentDecisionReceipt) {
	evidenceID := receipt.Decision.ProducedEvidenceID
	if evidenceID == "" {
		evidenceID = "none"
	}
	_, _ = fmt.Fprintf(out, "Offline decision: %s assessment=%s disposition=%s replayed=%t aggregate_version=%s evidence=%s\n",
		safeText(receipt.Decision.DecisionID), safeText(receipt.AssessmentID), safeText(receipt.Decision.Disposition), receipt.Replayed,
		safeText(string(receipt.AggregateVersion)), safeText(evidenceID))
}
