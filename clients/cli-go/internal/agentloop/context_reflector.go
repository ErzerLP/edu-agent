package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

const (
	reflectorToolName        = "record_session_reflections"
	maxReflectionsPerRun     = 64
	maxReflectionContentRune = 1200
)

type reflectionDraft struct {
	Content   string
	Kind      ReflectionKind
	Support   []CoverageEdge
	Authority AuthorityClass
	Freshness FreshnessClass
}

type reflectorResult struct {
	Reflections []reflectionDraft
}

type reflectorRecordArgs struct {
	Reflections []reflectorReflectionArg `json:"reflections"`
}

type reflectorReflectionArg struct {
	Content   string                 `json:"content"`
	Kind      ReflectionKind         `json:"kind"`
	Support   []reflectorCoverageArg `json:"support"`
	Authority AuthorityClass         `json:"authority"`
	Freshness FreshnessClass         `json:"freshness"`
}

type reflectorCoverageArg struct {
	ObservationID string           `json:"observation_id"`
	Fidelity      CoverageFidelity `json:"fidelity"`
}

func runReflector(ctx context.Context, model Model, estimator TokenEstimator, contextWindow int, snapshot reflectorSnapshot) (reflectorResult, error) {
	if len(snapshot.Observations) == 0 {
		return reflectorResult{}, nil
	}
	request := modelclient.Request{
		Messages: []modelclient.Message{
			{Role: "system", Content: reflectorSystemPrompt},
			{Role: "user", Content: renderReflectorInput(snapshot, estimator, clampInt(divideRoundUp(contextWindow*20, 100), 512, 8192))},
		},
		Tools:     []modelclient.Tool{reflectorRecordTool()},
		MaxTokens: min(2048, max(1, contextWindow/4)),
	}
	response, err := model.Complete(ctx, request)
	if err != nil {
		return reflectorResult{}, fmt.Errorf("context reflector model request: %w", err)
	}
	return validateReflectorResponse(response.Message, snapshot)
}

func reflectorRecordTool() modelclient.Tool {
	return tool(reflectorToolName, "把活跃观察提炼为耐久会话反思，并明确 partial 或 exact 支持边。", `{"type":"object","properties":{"reflections":{"type":"array","maxItems":64,"items":{"type":"object","properties":{"content":{"type":"string","maxLength":1200},"kind":{"type":"string","enum":["user_intent","user_constraint","correction","decision","completion","open_blocker","server_state","preference_flow"]},"support":{"type":"array","minItems":1,"maxItems":32,"items":{"type":"object","properties":{"observation_id":{"type":"string"},"fidelity":{"type":"string","enum":["partial","exact"]}},"required":["observation_id","fidelity"],"additionalProperties":false}},"authority":{"type":"string","enum":["session_statement","server_snapshot","server_reference"]},"freshness":{"type":"string","enum":["session_current","historical_snapshot"]}},"required":["content","kind","support","authority","freshness"],"additionalProperties":false}}},"required":["reflections"],"additionalProperties":false}`)
}

const reflectorSystemPrompt = `你是有界会话 Reflector。你只能读取给出的 Observation 和 Reflection，绝不能读取或推断原始聊天。你只能调用一次 record_session_reflections。只提炼需要跨多轮保留的用户目标、约束、纠正、已确认决定及理由、不可重复的完成结果、未解决 blocker、偏好流程状态和带版本的服务端引用。support 必须引用活跃 observation；fidelity 只能是 partial 或 exact。只有内容完整保留观察的关键语义时才可标 exact；多个 partial 永远不能合并成 exact。server snapshot 只能是历史状态或 server_reference，不能声称仍然当前。没有耐久内容时返回空内容且不要调用工具。`

func renderReflectorInput(snapshot reflectorSnapshot, estimator TokenEstimator, tokenLimit int) string {
	var builder strings.Builder
	builder.WriteString("仅根据以下 observation/reflection JSON 行生成耐久反思。\n")
	for _, reflection := range snapshot.Reflections {
		writeBoundedJSONLine(&builder, map[string]any{
			"record_type": "reflection", "id": reflection.ID, "content": reflection.Content,
			"kind": reflection.Kind, "authority": reflection.Authority, "freshness": reflection.Freshness,
		}, estimator, tokenLimit)
	}
	for _, observation := range snapshot.Observations {
		writeBoundedJSONLine(&builder, map[string]any{
			"record_type": "observation", "id": observation.ID, "content": observation.Content,
			"kind": observation.Kind, "relevance": observation.Relevance,
			"authority": observation.Authority, "freshness": observation.Freshness,
		}, estimator, tokenLimit)
	}
	return builder.String()
}

func validateReflectorResponse(message modelclient.Message, snapshot reflectorSnapshot) (reflectorResult, error) {
	if len(message.ToolCalls) == 0 {
		if strings.TrimSpace(message.Content) == "" {
			return reflectorResult{}, nil
		}
		return reflectorResult{}, errors.New("context reflector returned text instead of the record tool")
	}
	if strings.TrimSpace(message.Content) != "" || len(message.ToolCalls) != 1 {
		return reflectorResult{}, errors.New("context reflector exceeded the single-call protocol")
	}
	call := message.ToolCalls[0]
	if call.Type != "function" || call.Function.Name != reflectorToolName {
		return reflectorResult{}, errors.New("context reflector called a non-reflector tool")
	}
	var args reflectorRecordArgs
	if err := decodeArguments(call.Function.Arguments, &args); err != nil {
		return reflectorResult{}, fmt.Errorf("context reflector arguments: %w", err)
	}
	if len(args.Reflections) > maxReflectionsPerRun {
		return reflectorResult{}, errors.New("context reflector returned too many reflections")
	}
	active := make(map[string]Observation, len(snapshot.Observations))
	for _, observation := range snapshot.Observations {
		active[observation.ID] = observation
	}
	result := reflectorResult{Reflections: make([]reflectionDraft, 0, len(args.Reflections))}
	for _, raw := range args.Reflections {
		draft, err := validateReflectionArg(raw, active)
		if err != nil {
			return reflectorResult{}, err
		}
		result.Reflections = append(result.Reflections, draft)
	}
	return result, nil
}

func validateReflectionArg(raw reflectorReflectionArg, active map[string]Observation) (reflectionDraft, error) {
	raw.Content = strings.TrimSpace(raw.Content)
	if !validSingleLine(raw.Content, maxReflectionContentRune) || looksLikeUnredactedSecret(raw.Content) || !validReflectionKind(raw.Kind) {
		return reflectionDraft{}, errors.New("context reflector returned unsafe reflection content or kind")
	}
	if len(raw.Support) == 0 || len(raw.Support) > 32 {
		return reflectionDraft{}, errors.New("context reflector returned invalid support cardinality")
	}
	seen := make(map[string]struct{}, len(raw.Support))
	support := make([]CoverageEdge, 0, len(raw.Support))
	allSession, allServer := true, true
	for _, rawEdge := range raw.Support {
		observation, exists := active[rawEdge.ObservationID]
		if !exists || rawEdge.Fidelity != CoveragePartial && rawEdge.Fidelity != CoverageExact {
			return reflectionDraft{}, errors.New("context reflector returned invalid support")
		}
		if _, duplicate := seen[rawEdge.ObservationID]; duplicate {
			return reflectionDraft{}, errors.New("context reflector repeated support")
		}
		seen[rawEdge.ObservationID] = struct{}{}
		allSession = allSession && observation.Authority == AuthoritySessionStatement
		allServer = allServer && (observation.Authority == AuthorityServerSnapshot || observation.Authority == AuthorityServerReference)
		support = append(support, CoverageEdge{ObservationID: rawEdge.ObservationID, Fidelity: rawEdge.Fidelity})
	}
	if allSession {
		if raw.Authority != AuthoritySessionStatement || raw.Freshness != FreshnessSessionCurrent || raw.Kind == ReflectionServerState {
			return reflectionDraft{}, errors.New("context reflector assigned invalid session authority")
		}
	} else if allServer {
		if raw.Authority != AuthorityServerSnapshot && raw.Authority != AuthorityServerReference || raw.Freshness != FreshnessHistorical || raw.Kind != ReflectionServerState {
			return reflectionDraft{}, errors.New("context reflector assigned invalid server authority")
		}
	} else {
		return reflectionDraft{}, errors.New("context reflector mixed incompatible authority classes")
	}
	sort.Slice(support, func(i, j int) bool { return support[i].ObservationID < support[j].ObservationID })
	return reflectionDraft{
		Content: raw.Content, Kind: raw.Kind, Support: support,
		Authority: raw.Authority, Freshness: raw.Freshness,
	}, nil
}

func validReflectionKind(value ReflectionKind) bool {
	switch value {
	case ReflectionUserIntent, ReflectionUserConstraint, ReflectionCorrection, ReflectionDecision,
		ReflectionCompletion, ReflectionOpenBlocker, ReflectionServerState, ReflectionPreferenceFlow:
		return true
	default:
		return false
	}
}

func marshalReflectorArguments(value reflectorRecordArgs) string {
	data, _ := json.Marshal(value)
	return string(data)
}
