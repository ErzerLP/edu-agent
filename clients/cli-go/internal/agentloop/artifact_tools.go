package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localartifact"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func isLocalPrivateTool(name string) bool {
	return isLocalExecutionTool(name) || name == "artifact" || name == "apply_patch"
}

func artifactTool() modelclient.Tool {
	return tool("artifact", "本Session只读diff/清单/回执：list，read按原始字节offset/limit，search字面needle。b_追加日志：plan非执行，pending无actual=unknown，未开始=not_started；不重放。", `{"type":"object","properties":{"action":{"type":"string","enum":["list","read","search"]},"id":{"type":"string","minLength":1,"maxLength":128},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1,"maximum":65536},"needle":{"type":"string","minLength":1,"maxLength":512}},"required":["action"],"additionalProperties":false}`)
}

type artifactToolArgs struct {
	Action, ID, Needle string
	Offset             int64
	Limit              int
}
type artifactToolResult struct {
	Code               string
	Items              []localartifact.Info
	Offset, NextOffset int64
	More               bool
	Page               *localartifact.Page
	Search             *localartifact.SearchPage
}

func decodeArtifactArgs(raw string) (artifactToolArgs, error) {
	args := artifactToolArgs{Limit: 4096}
	fields, err := localArguments(raw, "artifact")
	if err != nil {
		return args, err
	}
	if json.Unmarshal(fields["action"], &args.Action) != nil {
		return args, errors.New("invalid_arguments")
	}
	allowed := map[string]any{"action": &args.Action, "offset": &args.Offset, "limit": &args.Limit}
	switch args.Action {
	case "list":
		args.Limit = 20
	case "read":
		allowed["id"] = &args.ID
	case "search":
		args.Limit = 20
		allowed["id"] = &args.ID
		allowed["needle"] = &args.Needle
	default:
		return args, errors.New("invalid_arguments")
	}
	if err := decodeLocalFields(fields, allowed); err != nil {
		return args, err
	}
	if args.Offset < 0 || args.Limit < 1 || args.Limit > 65536 || args.Action != "read" && args.Limit > 100 {
		return args, errors.New("invalid_arguments")
	}
	if args.Action != "list" && (args.ID == "" || len(args.ID) > 128) {
		return args, errors.New("invalid_arguments")
	}
	if args.Action == "search" && (len(args.Needle) < 1 || len(args.Needle) > 512 || !utf8.ValidString(args.Needle)) {
		return args, errors.New("invalid_arguments")
	}
	return args, nil
}

func artifactErrorCode(err error) string {
	var failure *localartifact.Error
	if errors.As(err, &failure) {
		return failure.Code
	}
	var batch *fileeffects.BatchError
	if errors.As(err, &batch) {
		return batch.Code
	}
	return "artifact_unavailable"
}

func (s *Session) executeArtifactTool(ctx context.Context, call modelclient.ToolCall) artifactToolResult {
	args, err := decodeArtifactArgs(call.Function.Arguments)
	if err != nil {
		return artifactToolResult{Code: "invalid_arguments"}
	}
	catalog := s.artifactCatalog()
	if catalog.Artifacts == nil || catalog.Owner == "" {
		return artifactToolResult{Code: "artifact_unavailable"}
	}
	if err := ctx.Err(); err != nil {
		return artifactToolResult{Code: "artifact_canceled"}
	}
	switch args.Action {
	case "list":
		items, err := catalog.List(ctx)
		if err != nil {
			return artifactToolResult{Code: artifactErrorCode(err)}
		}
		if args.Offset > int64(len(items)) {
			return artifactToolResult{Code: "invalid_arguments"}
		}
		end := min(int64(len(items)), args.Offset+int64(args.Limit))
		return artifactToolResult{Items: items[args.Offset:end], Offset: args.Offset, NextOffset: end, More: end < int64(len(items))}
	case "read":
		page, err := catalog.Read(ctx, args.ID, args.Offset, args.Limit)
		if err != nil {
			return artifactToolResult{Code: artifactErrorCode(err)}
		}
		return artifactToolResult{Page: &page}
	default:
		page, err := catalog.Search(ctx, args.ID, args.Needle, args.Offset, args.Limit)
		if err != nil {
			return artifactToolResult{Code: artifactErrorCode(err)}
		}
		return artifactToolResult{Search: &page}
	}
}

func (s *Session) SetArtifactIdentity(owner string) error {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if s.contextRuntime.isClosed() {
		return ErrSessionClosed
	}
	if owner == "" || s.activeTurnID != "" {
		return ErrActiveTurn
	}
	if s.options.Artifacts != nil && len(s.options.Artifacts.List(s.options.ArtifactOwner)) != 0 {
		return errors.New("artifact owner already has results")
	}
	s.options.ArtifactOwner = owner
	return nil
}
