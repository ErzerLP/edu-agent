package command

import (
	"context"
	"fmt"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workbench"
)

// recordExplanation 返回正式讲解提案和应用后的会话，供两个展示入口复用。
func (a *App) recordExplanation(ctx context.Context, client APIClient, view api.SessionView) (api.SessionView, string, error) {
	step, err := currentRouteStep(view)
	if err != nil {
		return view, "", err
	}
	query := step.TeachingIntent
	if strings.TrimSpace(query) == "" {
		query = view.WorkItem.GoalRevision.Text
	}
	retrieval, err := a.retrieveForWorkItem(ctx, client, view, query, view.WorkItem.RouteRevision.KnowledgeRevisionID)
	if err != nil {
		return view, "", err
	}
	id, err := a.operationID()
	if err != nil {
		return view, "", err
	}
	r, err := proposalRequest(view, "explanation", retrieval, id)
	if err != nil {
		return view, "", err
	}
	proposal, fresh, stale, err := a.createProposalAndRefetch(ctx, client, view, r)
	if err != nil || stale {
		return fresh, "", err
	}
	if proposal.Text == nil {
		return view, "", fmt.Errorf("服务端讲解提案缺少正文")
	}
	id, err = a.operationID()
	if err != nil {
		return view, "", err
	}
	fresh, conflict, err := a.applyAndRefetch(ctx, client, fresh, api.ActionProposalExposureRequest{SessionOperation: sessionOperation(fresh, id), Action: "record_exposure", ProposalID: proposal.ProposalID, ExposureKind: "explanation"})
	if conflict {
		return fresh, "", err
	}
	return fresh, proposal.Text.Text, err
}

func assessmentFields(view api.SessionView) []workbench.Field {
	fields := []workbench.Field{{ID: "reason", Label: "覆盖原因"}}
	if view.WorkItem == nil || view.WorkItem.AssessmentDecision == nil {
		return fields
	}
	for i, item := range view.WorkItem.AssessmentDecision.Items {
		fields = append(fields, workbench.Field{ID: "conclusion:" + item.RubricItemID, Label: fmt.Sprintf("评分项 %d 结论 pass/partial/fail", i+1), Value: item.Conclusion, Choices: []string{"pass", "partial", "fail"}}, workbench.Field{ID: "candidate:" + item.RubricItemID, Label: fmt.Sprintf("评分项 %d 误区候选", i+1), Value: item.MisconceptionCandidate})
	}
	return fields
}

func (a *App) workbenchAssessment(ctx context.Context, client APIClient, view api.SessionView, req workbench.Request) (api.SessionView, error) {
	action := strings.TrimPrefix(req.Action, "assessment_")
	w := view.WorkItem
	if w == nil || w.Assessment == nil || w.AssessmentDecision == nil || !allowed(w.AllowedAssessmentDecisions, action) {
		return view, fmt.Errorf("服务端未允许此评估处置")
	}
	id, err := a.operationID()
	if err != nil {
		return view, err
	}
	base := sessionOperation(view, id)
	if action == "void" {
		if strings.TrimSpace(req.Text) == "" {
			return view, fmt.Errorf("作废评估必须填写原因")
		}
		return a.decideAndRefetch(ctx, client, view, api.AssessmentVoidRequest{SessionOperation: base, Kind: "void", ExpectedDispositionVersion: w.AssessmentDecision.Version, Reason: strings.TrimSpace(req.Text)})
	}
	if strings.TrimSpace(req.Values["reason"]) == "" {
		return view, fmt.Errorf("覆盖评估必须填写原因")
	}
	items, err := assessmentOverrideItems(*w.Assessment, *w.AssessmentDecision, func(source, current api.AssessmentItem) (string, string, error) {
		return req.Values["conclusion:"+source.RubricItemID], req.Values["candidate:"+source.RubricItemID], nil
	})
	if err != nil {
		return view, err
	}
	return a.decideAndRefetch(ctx, client, view, api.AssessmentOverrideRequest{SessionOperation: base, Kind: "override", ExpectedDispositionVersion: w.AssessmentDecision.Version, Reason: strings.TrimSpace(req.Values["reason"]), Items: items})
}
