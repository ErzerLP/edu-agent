package command

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

const maxKnowledgeMaintenanceRequestBytes = 16 << 20

type knowledgeMaintenanceAPIClient interface {
	CreateKnowledgeMaintenanceProposal(context.Context, api.KnowledgeMaintenanceProposalRequest) (api.KnowledgeMaintenanceProposal, error)
	CreateKnowledgeMaintenanceRollback(context.Context, api.KnowledgeMaintenanceRollbackRequest) (api.KnowledgeMaintenanceProposal, error)
	KnowledgeMaintenanceProposals(context.Context, string, string, int) (api.KnowledgeMaintenanceProposalPage, error)
	KnowledgeMaintenanceProposal(context.Context, string) (api.KnowledgeMaintenanceProposal, error)
	DecideKnowledgeMaintenanceProposal(context.Context, string, string, api.KnowledgeMaintenanceDecisionRequest) (api.KnowledgeMaintenanceProposal, error)
	EvidenceCarryovers(context.Context, string, string, int) (api.EvidenceCarryoverPage, error)
	EvidenceCarryover(context.Context, string) (api.EvidenceCarryoverProposal, error)
	DecideEvidenceCarryover(context.Context, string, string, api.EvidenceCarryoverDecisionRequest) (api.EvidenceCarryoverProposal, error)
}

func asKnowledgeMaintenanceClient(value APIClient) (knowledgeMaintenanceAPIClient, error) {
	client, ok := value.(knowledgeMaintenanceAPIClient)
	if !ok {
		return nil, commandError("internal_error", "the online client lacks knowledge maintenance support", "check the CLI installation", ExitInternal)
	}
	return client, nil
}

func (a *App) RunKnowledgeMaintenance(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return knowledgeMaintenanceUsage("knowledge maintenance requires propose, rollback, proposals, proposal, approve, reject, or carryovers")
	}
	switch args[0] {
	case "propose":
		return a.runKnowledgeMaintenancePropose(ctx, args[1:])
	case "rollback":
		return a.runKnowledgeMaintenanceRollback(ctx, args[1:])
	case "proposals":
		return a.runKnowledgeMaintenanceProposals(ctx, args[1:])
	case "proposal":
		return a.runKnowledgeMaintenanceProposal(ctx, args[1:])
	case "approve", "reject":
		return a.runKnowledgeMaintenanceDecision(ctx, args[0], args[1:])
	case "carryovers":
		return a.runEvidenceCarryovers(ctx, args[1:])
	default:
		return knowledgeMaintenanceUsage("unknown knowledge maintenance command " + args[0])
	}
}

func (a *App) runKnowledgeMaintenancePropose(ctx context.Context, args []string) error {
	set := newFlagSet("knowledge maintenance propose")
	var flags onlineFlags
	var requestFile string
	addOnlineFlags(set, &flags)
	set.StringVar(&requestFile, "request-file", "", "strict JSON proposal request")
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 || strings.TrimSpace(requestFile) == "" {
		return commandError("usage", "knowledge maintenance propose requires --request-file", "run edu-agent knowledge maintenance propose --request-file FILE", ExitInput)
	}
	data, err := readKnowledgeMaintenanceRequestFile(requestFile)
	if err != nil {
		return err
	}
	request, err := api.DecodeKnowledgeMaintenanceProposalRequest(data)
	if err != nil {
		return commandError("invalid_knowledge_maintenance_request", "proposal request must be closed, complete, and valid JSON", "remove unknown, actor, computed, duplicate, or invalid fields", ExitInput)
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	client, err := asKnowledgeMaintenanceClient(online.client)
	if err != nil {
		return err
	}
	proposal, err := client.CreateKnowledgeMaintenanceProposal(ctx, request)
	if err != nil {
		return mapAPIError(err)
	}
	return printKnowledgeMaintenanceProposalSummary(a.Out, proposal)
}

func (a *App) runKnowledgeMaintenanceRollback(ctx context.Context, args []string) error {
	set := newFlagSet("knowledge maintenance rollback")
	var flags onlineFlags
	var requestFile string
	addOnlineFlags(set, &flags)
	set.StringVar(&requestFile, "request-file", "", "strict JSON rollback request")
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 || strings.TrimSpace(requestFile) == "" {
		return commandError("usage", "knowledge maintenance rollback requires --request-file", "run edu-agent knowledge maintenance rollback --request-file FILE", ExitInput)
	}
	data, err := readKnowledgeMaintenanceRequestFile(requestFile)
	if err != nil {
		return err
	}
	request, err := api.DecodeKnowledgeMaintenanceRollbackRequest(data)
	if err != nil {
		return commandError("invalid_knowledge_maintenance_request", "rollback request must be closed, complete, and valid JSON", "remove unknown, actor, computed, duplicate, or invalid fields", ExitInput)
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	client, err := asKnowledgeMaintenanceClient(online.client)
	if err != nil {
		return err
	}
	proposal, err := client.CreateKnowledgeMaintenanceRollback(ctx, request)
	if err != nil {
		return mapAPIError(err)
	}
	return printKnowledgeMaintenanceProposalSummary(a.Out, proposal)
}

func (a *App) runKnowledgeMaintenanceProposals(ctx context.Context, args []string) error {
	set := newFlagSet("knowledge maintenance proposals")
	var flags onlineFlags
	var status, cursor string
	var limit int
	addOnlineFlags(set, &flags)
	set.StringVar(&status, "status", "", "proposal status")
	set.StringVar(&cursor, "cursor", "", "proposal cursor")
	set.IntVar(&limit, "limit", 0, "proposal page size")
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 || !validKnowledgeMaintenanceListInput(status, cursor, limit) {
		return commandError("usage", "knowledge maintenance proposals has invalid status or pagination", "use status open, applied, rejected, stale, or redacted; and limit 1 through 100", ExitInput)
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	client, err := asKnowledgeMaintenanceClient(online.client)
	if err != nil {
		return err
	}
	page, err := client.KnowledgeMaintenanceProposals(ctx, status, cursor, limit)
	if err != nil {
		return mapAPIError(err)
	}
	for _, proposal := range page.Items {
		_, _ = fmt.Fprintf(a.Out, "Proposal: id=%s request=%s kind=%s status=%s base=%s risk=%s created_by=%s generation=%d applied_revision=%s stale_reason=%s\n",
			safeText(proposal.ProposalID), safeText(proposal.RequestID), safeText(proposal.Kind), safeText(proposal.Status), safeText(proposal.BaseRevisionID), safeText(proposal.Risk.Level), safeText(proposal.CreatedByDeviceID), proposal.KnowledgeGeneration, safeText(proposal.AppliedRevisionID), safeText(knowledgeMaintenanceStaleReason(proposal)))
	}
	_, err = fmt.Fprintf(a.Out, "Next cursor: %s\n", safeText(page.NextCursor))
	return err
}

func (a *App) runKnowledgeMaintenanceProposal(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return commandError("usage", "knowledge maintenance proposal requires one proposal ID", "run edu-agent knowledge maintenance proposal <proposal-id>", ExitInput)
	}
	proposalID := strings.ToLower(strings.TrimSpace(args[0]))
	set := newFlagSet("knowledge maintenance proposal")
	var flags onlineFlags
	addOnlineFlags(set, &flags)
	if !validNotesyncUUID(proposalID) || set.Parse(args[1:]) != nil || len(set.Args()) != 0 {
		return commandError("usage", "knowledge maintenance proposal requires one valid proposal ID and optional connection flags", "run edu-agent knowledge maintenance proposal <proposal-id>", ExitInput)
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	client, err := asKnowledgeMaintenanceClient(online.client)
	if err != nil {
		return err
	}
	proposal, err := client.KnowledgeMaintenanceProposal(ctx, proposalID)
	if err != nil {
		return mapAPIError(err)
	}
	return printKnowledgeMaintenanceProposal(a.Out, proposal)
}

func (a *App) runKnowledgeMaintenanceDecision(ctx context.Context, decision string, args []string) error {
	if len(args) == 0 {
		return commandError("usage", "knowledge maintenance "+decision+" requires one proposal ID", "run edu-agent knowledge maintenance "+decision+" <proposal-id> --operation-id UUID [--reason TEXT]", ExitInput)
	}
	proposalID := strings.ToLower(strings.TrimSpace(args[0]))
	set := newFlagSet("knowledge maintenance " + decision)
	var flags onlineFlags
	var operationID, reason string
	addOnlineFlags(set, &flags)
	set.StringVar(&operationID, "operation-id", "", "idempotent operation UUID")
	set.StringVar(&reason, "reason", "", "decision reason")
	if !validNotesyncUUID(proposalID) || set.Parse(args[1:]) != nil || len(set.Args()) != 0 {
		return commandError("usage", "knowledge maintenance "+decision+" requires a valid proposal ID and flags", "run edu-agent knowledge maintenance "+decision+" <proposal-id> --operation-id UUID [--reason TEXT]", ExitInput)
	}
	operationID = strings.ToLower(strings.TrimSpace(operationID))
	reason = strings.TrimSpace(reason)
	if !validNotesyncUUID(operationID) || !utf8.ValidString(reason) || len([]rune(reason)) > 4000 || strings.IndexByte(reason, 0) >= 0 {
		return commandError("invalid_knowledge_maintenance_decision", "operation ID or reason is invalid", "use a UUID and a UTF-8 reason up to 4000 characters", ExitInput)
	}
	if reason == "" {
		if decision == "approve" {
			reason = "approved by edu-agent Go CLI"
		} else {
			reason = "rejected by edu-agent Go CLI"
		}
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	client, err := asKnowledgeMaintenanceClient(online.client)
	if err != nil {
		return err
	}
	proposal, err := client.DecideKnowledgeMaintenanceProposal(ctx, proposalID, decision, api.KnowledgeMaintenanceDecisionRequest{OperationID: operationID, Reason: reason})
	if err != nil {
		return mapAPIError(err)
	}
	return printKnowledgeMaintenanceProposalSummary(a.Out, proposal)
}

func knowledgeMaintenanceUsage(detail string) error {
	return commandError("usage", detail, "run edu-agent knowledge maintenance propose, rollback, proposals, proposal, approve, reject, or carryovers", ExitInput)
}

func readKnowledgeMaintenanceRequestFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, commandError("invalid_knowledge_maintenance_request_file", "request JSON must be a readable regular file", "remove symlinks and select a regular UTF-8 file", ExitInput)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, commandError("invalid_knowledge_maintenance_request_file", "request JSON could not be opened", "check file permissions", ExitInput)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, commandError("invalid_knowledge_maintenance_request_file", "request JSON changed while being opened", "retry with a stable regular file", ExitInput)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxKnowledgeMaintenanceRequestBytes+1))
	if err != nil {
		return nil, commandError("invalid_knowledge_maintenance_request_file", "request JSON could not be read", "check file permissions", ExitInput)
	}
	if len(data) > maxKnowledgeMaintenanceRequestBytes {
		return nil, commandError("payload_too_large", "request JSON exceeds 16 MiB", "reduce the request file", ExitInput)
	}
	if !utf8.Valid(data) {
		return nil, commandError("invalid_knowledge_maintenance_request_file", "request JSON is not UTF-8", "re-encode the request file as UTF-8", ExitInput)
	}
	return data, nil
}

func validKnowledgeMaintenanceListInput(status, cursor string, limit int) bool {
	switch status {
	case "", "open", "applied", "rejected", "stale", "redacted":
	default:
		return false
	}
	return utf8.ValidString(cursor) && len(cursor) <= 4096 && limit >= 0 && limit <= 100
}

func knowledgeMaintenanceStaleReason(value api.KnowledgeMaintenanceProposal) string {
	if value.Status == "stale" && value.Decision != nil {
		return value.Decision.Reason
	}
	return ""
}

func printKnowledgeMaintenanceProposalSummary(out interface{ Write([]byte) (int, error) }, value api.KnowledgeMaintenanceProposal) error {
	_, err := fmt.Fprintf(out, "Proposal ID: %s\nRequest ID: %s\nKind: %s\nStatus: %s\nBase revision: %s\nRisk: %s\nCreated by: %s\nKnowledge generation: %d\nApplied revision: %s\nStale reason: %s\n",
		safeText(value.ProposalID), safeText(value.RequestID), safeText(value.Kind), safeText(value.Status), safeText(value.BaseRevisionID), safeText(value.Risk.Level), safeText(value.CreatedByDeviceID), value.KnowledgeGeneration, safeText(value.AppliedRevisionID), safeText(knowledgeMaintenanceStaleReason(value)))
	return err
}

func printKnowledgeMaintenanceProposal(out interface{ Write([]byte) (int, error) }, value api.KnowledgeMaintenanceProposal) error {
	if err := printKnowledgeMaintenanceProposalSummary(out, value); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Current revision: %s\nRollback target: %s\nBasis hash: %s\nRedacted: %t\nCreated at: %s\nUpdated at: %s\n",
		safeText(value.CurrentRevisionID), safeText(value.RollbackTargetRevisionID), safeText(value.BasisHash), value.Redacted, value.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), value.UpdatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"))
	_, _ = fmt.Fprintf(out, "Policy versions: canonicalizer=%s identity=%s diff=%s risk=%s auto_apply=%s\n",
		safeText(value.CanonicalizerVersion), safeText(value.IdentityPolicyVersion), safeText(value.DiffVersion), safeText(value.RiskVersion), safeText(value.AutoApplyPolicyVersion))
	for _, source := range value.Sources {
		_, _ = fmt.Fprintf(out, "Source: kind=%s locator=%s title=%s sha256=%s\n", safeText(source.Kind), safeText(source.Locator), safeText(source.Title), safeText(source.SHA256))
	}
	for _, diff := range value.Diff {
		_, _ = fmt.Fprintf(out, "Diff: document=%s kind=%s before=%s after=%s truncated=%t local_body_only=%t changed_body_bytes=%d\n%s\n",
			safeText(diff.DocumentID), safeText(diff.Kind), safeText(diff.BeforePath), safeText(diff.AfterPath), diff.Truncated, diff.LocalBodyOnly, diff.ChangedBodyBytes, safeText(diff.UnifiedDiff))
		printKnowledgeMaintenanceIDs(out, "Diff added nodes", diff.AddedNodeIDs)
		printKnowledgeMaintenanceIDs(out, "Diff removed nodes", diff.RemovedNodeIDs)
		printKnowledgeMaintenanceIDs(out, "Diff edited nodes", diff.EditedNodeIDs)
		printKnowledgeMaintenanceIDs(out, "Diff title nodes", diff.TitleNodeIDs)
		printKnowledgeMaintenanceIDs(out, "Diff structure nodes", diff.StructureNodeIDs)
	}
	printKnowledgeMaintenanceIDs(out, "Identity preserved documents", value.IdentityImpact.PreservedDocumentIDs)
	printKnowledgeMaintenanceIDs(out, "Identity added documents", value.IdentityImpact.AddedDocumentIDs)
	printKnowledgeMaintenanceIDs(out, "Identity removed documents", value.IdentityImpact.RemovedDocumentIDs)
	printKnowledgeMaintenanceIDs(out, "Identity moved documents", value.IdentityImpact.MovedDocumentIDs)
	printKnowledgeMaintenanceIDs(out, "Identity preserved nodes", value.IdentityImpact.PreservedNodeIDs)
	printKnowledgeMaintenanceIDs(out, "Identity added nodes", value.IdentityImpact.AddedNodeIDs)
	printKnowledgeMaintenanceIDs(out, "Identity removed nodes", value.IdentityImpact.RemovedNodeIDs)
	_, _ = fmt.Fprintf(out, "Identity uncertain: %t\nLineage impact: move=%t delete=%t restore=%t rollback=%t\n", value.IdentityImpact.Uncertain, value.LineageImpact.Move, value.LineageImpact.Delete, value.LineageImpact.Restore, value.LineageImpact.Rollback)
	for _, lineage := range value.LineageImpact.Lineages {
		members := make([]string, 0, len(lineage.Members))
		for _, member := range lineage.Members {
			members = append(members, member.Role+"="+member.NodeRevisionID)
		}
		sort.Strings(members)
		_, _ = fmt.Fprintf(out, "Lineage: id=%s action=%s revision=%s actor=%s policy=%s reason=%s members=%s\n", safeText(lineage.LineageID), safeText(lineage.Action), safeText(lineage.KnowledgeRevisionID), safeText(lineage.ActorDeviceID), safeText(lineage.PolicyVersion), safeText(lineage.Reason), safeText(strings.Join(members, ",")))
	}
	impact := value.AcceptedLearningEvidenceImpact
	_, _ = fmt.Fprintf(out, "Evidence impact: count=%d fingerprint=%s generation=%d\n", impact.Count, safeText(impact.Fingerprint), impact.Generation)
	for _, reference := range impact.References {
		_, _ = fmt.Fprintf(out, "Evidence: id=%s node_revision=%s knowledge_revision=%s\n", safeText(reference.EvidenceID), safeText(reference.NodeRevisionID), safeText(reference.KnowledgeRevisionID))
	}
	_, _ = fmt.Fprintf(out, "Risk detail: level=%s auto_apply=%t policy=%s reasons=%s\n", safeText(value.Risk.Level), value.Risk.AutoApply, safeText(value.Risk.PolicyVersion), safeText(strings.Join(value.Risk.Reasons, ",")))
	if value.Decision != nil {
		_, _ = fmt.Fprintf(out, "Decision: id=%s operation=%s requested=%s outcome=%s actor=%s reason=%s created_at=%s\n", safeText(value.Decision.DecisionID), safeText(value.Decision.OperationID), safeText(value.Decision.RequestedDecision), safeText(value.Decision.Outcome), safeText(value.Decision.ActorDeviceID), safeText(value.Decision.Reason), value.Decision.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"))
	}
	if value.Origin != nil {
		_, _ = fmt.Fprintf(out, "Origin: version=%s kind=%s proposal=%s base=%s rollback_target=%s basis_hash=%s\n", safeText(value.Origin.Version), safeText(value.Origin.Kind), safeText(value.Origin.ProposalID), safeText(value.Origin.BaseRevisionID), safeText(value.Origin.RollbackTargetRevisionID), safeText(value.Origin.BasisHash))
	}
	return nil
}

func printKnowledgeMaintenanceIDs(out interface{ Write([]byte) (int, error) }, label string, values []string) {
	values = append([]string(nil), values...)
	sort.Strings(values)
	_, _ = fmt.Fprintf(out, "%s: %s\n", label, safeText(strings.Join(values, ",")))
}
