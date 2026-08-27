package command

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

func (a *App) runEvidenceCarryovers(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return evidenceCarryoverUsage("knowledge maintenance carryovers requires list, get, approve, or reject")
	}
	switch args[0] {
	case "list":
		return a.runEvidenceCarryoverList(ctx, args[1:])
	case "get":
		return a.runEvidenceCarryoverGet(ctx, args[1:])
	case "approve", "reject":
		return a.runEvidenceCarryoverDecision(ctx, args[0], args[1:])
	default:
		return evidenceCarryoverUsage("unknown evidence carryover command " + args[0])
	}
}

func (a *App) runEvidenceCarryoverList(ctx context.Context, args []string) error {
	set := newFlagSet("knowledge maintenance carryovers list")
	var flags onlineFlags
	status, limit := "all", 50
	var cursor string
	addOnlineFlags(set, &flags)
	set.StringVar(&status, "status", status, "all, open, approved, rejected, stale, or redacted")
	set.StringVar(&cursor, "cursor", "", "opaque generation-bound cursor")
	set.IntVar(&limit, "limit", limit, "page size from 1 through 100")
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 || api.ValidateEvidenceCarryoverQuery(status, cursor, limit) != nil {
		return commandError("usage", "evidence carryover list has invalid status or pagination", "run edu-agent knowledge maintenance carryovers list --status all --limit 50 [--cursor CURSOR]", ExitInput)
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	client, err := asKnowledgeMaintenanceClient(online.client)
	if err != nil {
		return err
	}
	page, err := client.EvidenceCarryovers(ctx, status, cursor, limit)
	if err != nil {
		return mapAPIError(err)
	}
	return printEvidenceCarryoverJSON(a.Out, page)
}

func (a *App) runEvidenceCarryoverGet(ctx context.Context, args []string) error {
	set := newFlagSet("knowledge maintenance carryovers get")
	var flags onlineFlags
	var proposalID string
	addOnlineFlags(set, &flags)
	set.StringVar(&proposalID, "proposal-id", "", "canonical proposal UUID")
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 || api.ValidateEvidenceCarryoverProposalID(proposalID) != nil {
		return commandError("usage", "evidence carryover get requires --proposal-id with a canonical UUID", "run edu-agent knowledge maintenance carryovers get --proposal-id UUID", ExitInput)
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	client, err := asKnowledgeMaintenanceClient(online.client)
	if err != nil {
		return err
	}
	proposal, err := client.EvidenceCarryover(ctx, proposalID)
	if err != nil {
		return mapAPIError(err)
	}
	return printEvidenceCarryoverJSON(a.Out, proposal)
}

func (a *App) runEvidenceCarryoverDecision(ctx context.Context, decision string, args []string) error {
	set := newFlagSet("knowledge maintenance carryovers " + decision)
	var flags onlineFlags
	var proposalID, operationID, reason string
	addOnlineFlags(set, &flags)
	set.StringVar(&proposalID, "proposal-id", "", "canonical proposal UUID")
	set.StringVar(&operationID, "operation-id", "", "canonical idempotency UUID")
	set.StringVar(&reason, "reason", "", "non-empty decision reason")
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 || reason != strings.TrimSpace(reason) || !utf8.ValidString(reason) {
		return invalidEvidenceCarryoverDecision(decision)
	}
	request := api.EvidenceCarryoverDecisionRequest{OperationID: operationID, Decision: decision, Reason: reason}
	if api.ValidateEvidenceCarryoverDecisionRequest(proposalID, decision, request) != nil {
		return invalidEvidenceCarryoverDecision(decision)
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	client, err := asKnowledgeMaintenanceClient(online.client)
	if err != nil {
		return err
	}
	proposal, err := client.DecideEvidenceCarryover(ctx, proposalID, decision, request)
	if err != nil {
		return mapAPIError(err)
	}
	return printEvidenceCarryoverJSON(a.Out, proposal)
}

func invalidEvidenceCarryoverDecision(decision string) error {
	return commandError("usage", "evidence carryover "+decision+" requires canonical proposal and operation UUIDs plus a non-empty reason", "run edu-agent knowledge maintenance carryovers "+decision+" --proposal-id UUID --operation-id UUID --reason TEXT", ExitInput)
}

func evidenceCarryoverUsage(detail string) error {
	return commandError("usage", detail, "run edu-agent knowledge maintenance carryovers list|get|approve|reject", ExitInput)
}

func printEvidenceCarryoverJSON(out interface{ Write([]byte) (int, error) }, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return commandError("response_encoding_failed", "the validated carryover response could not be encoded", "check the CLI installation", ExitInternal)
	}
	data = append(data, '\n')
	_, err = out.Write(data)
	return err
}
