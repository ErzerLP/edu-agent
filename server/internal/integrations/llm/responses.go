package llm

import "encoding/json"

// Some Responses gateways reject object/array members in a type array. Express
// unions in the client's recursive schema subset as equivalent anyOf branches.
// This modifies only the decoded wire schema; validation uses the original bytes.
func responsesSchema(schema any) any {
	node, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	if properties, ok := node["properties"].(map[string]any); ok {
		for name, child := range properties {
			properties[name] = responsesSchema(child)
		}
	}
	if items, ok := node["items"]; ok {
		node["items"] = responsesSchema(items)
	}
	if types, ok := node["type"].([]any); ok {
		branches := make([]any, 0, len(types))
		for _, kind := range types {
			branch := make(map[string]any, len(node))
			for name, value := range node {
				branch[name] = value
			}
			branch["type"] = kind
			branches = append(branches, branch)
		}
		return map[string]any{"anyOf": branches}
	}
	return node
}

func decodeResponse(decoder *json.Decoder) ([]byte, TokenUsage, error) {
	var envelope struct {
		Status            string `json:"status"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Output []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens         int `json:"input_tokens"`
			OutputTokens        int `json:"output_tokens"`
			TotalTokens         int `json:"total_tokens"`
			OutputTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if err := decoder.Decode(&envelope); err != nil {
		return nil, TokenUsage{}, &Error{Category: ErrorInvalidResponse, Cause: err}
	}
	if envelope.Status != "completed" {
		category := ErrorResponseNotCompleted
		if envelope.Status == "incomplete" && envelope.IncompleteDetails.Reason == "max_output_tokens" {
			category = ErrorOutputBudgetExceeded
		}
		return nil, TokenUsage{}, &Error{Category: category}
	}
	var content []byte
	for _, output := range envelope.Output {
		for _, part := range output.Content {
			if part.Type == "output_text" {
				content = append(content, part.Text...)
			}
		}
	}
	if len(content) == 0 {
		return nil, TokenUsage{}, &Error{Category: ErrorInvalidResponse}
	}
	return content, TokenUsage{
		InputTokens:     envelope.Usage.InputTokens,
		OutputTokens:    envelope.Usage.OutputTokens,
		ReasoningTokens: envelope.Usage.OutputTokensDetails.ReasoningTokens,
		TotalTokens:     envelope.Usage.TotalTokens,
	}, nil
}
