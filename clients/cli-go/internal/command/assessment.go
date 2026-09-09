package command

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

func (a *App) runAssessment(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return commandError("usage", "assessment requires show, confirm, override, or void", "run edu-agent assessment show", ExitInput)
	}
	kind := args[0]
	if kind != "show" && kind != "confirm" && kind != "override" && kind != "void" {
		return commandError("usage", "assessment action is unknown", "use show, confirm, override, or void", ExitInput)
	}
	set := newFlagSet("assessment " + kind)
	var flags onlineFlags
	var sessionID string
	set.StringVar(&sessionID, "session", "", "指定教学会话")
	addOnlineFlags(set, &flags)
	if err := set.Parse(args[1:]); err != nil || len(set.Args()) != 0 {
		return commandError("usage", "assessment accepts only connection flags", "run edu-agent assessment "+kind, ExitInput)
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	var view api.SessionView
	var active bool
	if sessionID != "" {
		view, err = refetchSession(ctx, online.client, sessionID)
		active = err == nil
	} else {
		view, active, err = a.currentSession(ctx, online.client)
	}
	if err != nil {
		return err
	}
	if !active || view.WorkItem == nil || view.WorkItem.Assessment == nil || view.WorkItem.AssessmentDecision == nil {
		return commandError("assessment_not_found", "the current work item has no assessment", "continue learning until Feedback or inspect another session by API", ExitConflict)
	}
	printProjectionWarning(a.Err, view.Metadata)
	if kind == "show" {
		printAssessment(a.Out, *view.WorkItem.Assessment, *view.WorkItem.AssessmentDecision, view.WorkItem.AllowedAssessmentDecisions)
		return nil
	}
	decision := view.WorkItem.AssessmentDecision
	if decision.Disposition != "provisional" && kind == "confirm" {
		return commandError("assessment_disposition_conflict", "only a current provisional assessment can be confirmed", "run assessment show and use an allowed decision", ExitConflict)
	}
	if !allowed(view.WorkItem.AllowedAssessmentDecisions, kind) {
		return commandError("assessment_decision_not_allowed", "the server did not allow this decision", "run assessment show and use a displayed decision", ExitConflict)
	}
	operationID, err := a.operationID()
	if err != nil {
		return err
	}
	base := sessionOperation(view, operationID)
	var request api.AssessmentDecisionRequest
	switch kind {
	case "confirm":
		request = api.AssessmentConfirmRequest{SessionOperation: base, Kind: "confirm", ExpectedDispositionVersion: decision.Version}
	case "override":
		reason, readErr := a.Terminal.ReadLine(a.dashboardText("Override reason: ", "覆盖评估原因："))
		if readErr != nil || strings.TrimSpace(reason) == "" {
			return commandError("invalid_assessment_reason", "override requires a non-empty reason", "run assessment override again and explain the correction", ExitInput)
		}
		items, collectErr := a.collectAssessmentOverride(*view.WorkItem.Assessment, *decision)
		if collectErr != nil {
			return collectErr
		}
		request = api.AssessmentOverrideRequest{
			SessionOperation: base, Kind: "override", ExpectedDispositionVersion: decision.Version,
			Reason: strings.TrimSpace(reason), Items: items,
		}
	case "void":
		reason, readErr := a.Terminal.ReadLine(a.dashboardText("Void reason: ", "作废评估原因："))
		if readErr != nil || strings.TrimSpace(reason) == "" {
			return commandError("invalid_assessment_reason", "void requires a non-empty reason", "run assessment void again and explain why it should not count", ExitInput)
		}
		request = api.AssessmentVoidRequest{
			SessionOperation: base, Kind: "void", ExpectedDispositionVersion: decision.Version, Reason: strings.TrimSpace(reason),
		}
	}
	fresh, err := a.decideAndRefetch(ctx, online.client, view, request)
	if err != nil {
		return err
	}
	printProjectionWarning(a.Err, fresh.Metadata)
	if fresh.WorkItem != nil && fresh.WorkItem.Assessment != nil && fresh.WorkItem.AssessmentDecision != nil {
		printAssessment(a.Out, *fresh.WorkItem.Assessment, *fresh.WorkItem.AssessmentDecision, fresh.WorkItem.AllowedAssessmentDecisions)
	}
	return a.printDecisionProjection(ctx, online.client, view)
}

func (a *App) collectAssessmentOverride(artifact api.AssessmentArtifact, decision api.AssessmentDecision) ([]api.AssessmentItem, error) {
	return assessmentOverrideItems(artifact, decision, func(source, current api.AssessmentItem) (string, string, error) {
		_, _ = fmt.Fprintf(a.Out, "Rubric: %s current=%s\n", safeText(source.RubricItemID), safeText(current.Conclusion))
		conclusion, err := a.Terminal.ReadLine(a.dashboardText(fmt.Sprintf("Conclusion (pass/partial/fail) [%s]: ", safeText(current.Conclusion)), fmt.Sprintf("结论（pass/partial/fail）[%s]：", safeText(current.Conclusion))))
		if err != nil {
			return "", "", commandError("assessment_input_failed", "override conclusion could not be read", "run assessment override again", ExitInput)
		}
		conclusion, err = normalizeOverrideConclusion(conclusion, current.Conclusion)
		if err != nil {
			return "", "", err
		}
		candidate, err := a.Terminal.ReadLine(a.dashboardText("Misconception candidate (blank preserves current): ", "误区候选（留空保留当前值）："))
		if err != nil {
			return "", "", commandError("assessment_input_failed", "misconception candidate could not be read", "run assessment override again", ExitInput)
		}
		return conclusion, candidate, nil
	})
}

// assessmentOverrideItems 为脚本输入与工作台表单复用不可变证据校验及结论选择。
func assessmentOverrideItems(artifact api.AssessmentArtifact, decision api.AssessmentDecision, choose func(api.AssessmentItem, api.AssessmentItem) (string, string, error)) ([]api.AssessmentItem, error) {
	currentByRubric := map[string]api.AssessmentItem{}
	for _, item := range decision.Items {
		if _, ok := currentByRubric[item.RubricItemID]; ok {
			return nil, commandError("protocol_error", "评估包含重复评分项", "刷新评估", ExitInternal)
		}
		currentByRubric[item.RubricItemID] = item
	}
	if len(currentByRubric) != len(artifact.Items) {
		return nil, commandError("protocol_error", "评估处置与不可变证据不一致", "刷新评估", ExitInternal)
	}
	items := make([]api.AssessmentItem, len(artifact.Items))
	for i, source := range artifact.Items {
		current, ok := currentByRubric[source.RubricItemID]
		if !ok || !sameAssessmentSource(source, current) {
			return nil, commandError("protocol_error", "评估不可变证据不一致", "刷新评估", ExitInternal)
		}
		conclusion, candidate, err := choose(source, current)
		if err != nil {
			return nil, err
		}
		conclusion, err = normalizeOverrideConclusion(conclusion, current.Conclusion)
		if err != nil {
			return nil, err
		}
		item := source
		item.Conclusion = conclusion
		item.MisconceptionCandidate = current.MisconceptionCandidate
		if strings.TrimSpace(candidate) != "" {
			item.MisconceptionCandidate = strings.TrimSpace(candidate)
		}
		items[i] = item
	}
	return items, nil
}

func normalizeOverrideConclusion(value, current string) (string, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" && current != "unassessed" {
		value = current
	}
	if value != "pass" && value != "partial" && value != "fail" {
		return "", commandError("invalid_assessment_conclusion", "override conclusion must be pass, partial, or fail", "run assessment override again", ExitInput)
	}
	return value, nil
}

func sameAssessmentSource(artifact, decision api.AssessmentItem) bool {
	return artifact.RubricItemID == decision.RubricItemID && artifact.AnswerQuote == decision.AnswerQuote &&
		artifact.AnswerRange == decision.AnswerRange && artifact.AnswerQuoteSHA256 == decision.AnswerQuoteSHA256 &&
		artifact.KnowledgeReferenceID == decision.KnowledgeReferenceID && artifact.KnowledgeQuote == decision.KnowledgeQuote &&
		artifact.KnowledgeRange == decision.KnowledgeRange && artifact.KnowledgeQuoteSHA256 == decision.KnowledgeQuoteSHA256
}

func (a *App) decideAndRefetch(ctx context.Context, client APIClient, view api.SessionView, request api.AssessmentDecisionRequest) (api.SessionView, error) {
	assessmentID := view.WorkItem.Assessment.AssessmentID
	_, err := client.DecideAssessment(ctx, assessmentID, request)
	if ctx.Err() != nil {
		return view, ctx.Err()
	}
	if err != nil {
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && (apiErr.Code == "version_conflict" || apiErr.Code == "assessment_disposition_conflict") {
			fresh, fetchErr := refetchSession(ctx, client, view.Session.SessionID)
			if fetchErr != nil {
				return api.SessionView{}, mapAPIError(fetchErr)
			}
			if fresh.WorkItem != nil && fresh.WorkItem.Assessment != nil && fresh.WorkItem.AssessmentDecision != nil {
				printAssessment(a.Out, *fresh.WorkItem.Assessment, *fresh.WorkItem.AssessmentDecision, fresh.WorkItem.AllowedAssessmentDecisions)
			}
			return api.SessionView{}, mapAPIError(err)
		}
		return api.SessionView{}, mapAPIError(err)
	}
	fresh, err := refetchSession(ctx, client, view.Session.SessionID)
	if err != nil {
		return api.SessionView{}, mapAPIError(err)
	}
	return fresh, nil
}

func printAssessment(out interface{ Write([]byte) (int, error) }, artifact api.AssessmentArtifact, decision api.AssessmentDecision, allowedDecisions []string) {
	_, _ = fmt.Fprintf(out, "Result: assessment=%s disposition=%s decision_version=%d confidence=%d evidence=%s\n", safeText(artifact.AssessmentID), safeText(decision.Disposition), decision.Version, artifact.Confidence, safeText(decision.ProducedEvidenceID))
	for _, item := range decision.Items {
		_, _ = fmt.Fprintf(out, "Rubric: %s conclusion=%s", safeText(item.RubricItemID), safeText(item.Conclusion))
		if item.MisconceptionCandidate != "" {
			_, _ = fmt.Fprintf(out, " misconception=%s", safeText(item.MisconceptionCandidate))
		}
		_, _ = fmt.Fprintln(out)
	}
	if len(artifact.RiskFlags) > 0 {
		_, _ = fmt.Fprintf(out, "Risk: %s\n", safeText(strings.Join(artifact.RiskFlags, ",")))
	}
	_, _ = fmt.Fprintf(out, "Allowed decisions: %s\n", safeText(strings.Join(allowedDecisions, ",")))
	if decision.Disposition == "provisional" {
		_, _ = fmt.Fprintln(out, "Result: provisional; no mastery or review advancement is shown as accepted")
	}
}

func (a *App) printDecisionProjection(ctx context.Context, client APIClient, before api.SessionView) error {
	if before.WorkItem == nil || before.WorkItem.Activity == nil {
		return nil
	}
	nodeID := before.WorkItem.Activity.TargetNodeRevisionID
	node, err := client.Node(ctx, nodeID)
	if err != nil {
		return mapAPIError(err)
	}
	printProjectionWarning(a.Err, node.Metadata)
	_, _ = fmt.Fprintf(a.Out, "Node: %s mastery=%s evidence=%d pending_assessments=%d\n", safeText(nodeID), safeText(node.Node.Mastery.State), node.Node.Mastery.ValidEvidenceCount, node.Node.Mastery.PendingAssessments)
	evidence, err := a.evidencePage(ctx, client, "", defaultPageLimit, nodeID)
	if err != nil {
		return err
	}
	printProjectionWarning(a.Err, evidence.Metadata)
	reviews, err := a.reviewsPage(ctx, client, "", defaultPageLimit, nil)
	if err != nil {
		return err
	}
	printProjectionWarning(a.Err, reviews.Metadata)
	_, _ = fmt.Fprintf(a.Out, "Evidence: %d current-page items\nReviews: %d current-page items\n", len(evidence.Items), len(reviews.Items))
	return nil
}
