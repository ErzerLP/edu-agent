package agentloop

import (
	"encoding/json"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

// compactToolProse preserves the complete tool set and every schema constraint
// at small context windows. Tool names, arguments and execution policy do not
// change. System instructions still carry authority and authorization rules.
// Question display-width rules are kept in compact prose because they cannot
// be expressed by JSON Schema's character-length constraints.
const compactQuestionProse = "Answers aren't authorization. No secrets. Display columns: header<=36, question<=72, option label<=32, option description<=60."

func compactToolProse(tools []modelclient.Tool) []modelclient.Tool {
	result := append([]modelclient.Tool(nil), tools...)
	for index := range result {
		result[index].Function.Description = ""
		if result[index].Function.Name == "ask_user_question" {
			result[index].Function.Description = compactQuestionProse
		}
		var schema any
		if json.Unmarshal(result[index].Function.Parameters, &schema) != nil {
			continue
		}
		stripSchemaProse(schema)
		if encoded, err := json.Marshal(schema); err == nil {
			result[index].Function.Parameters = encoded
		}
	}
	return result
}

func onlyStringEnum(values []any) bool {
	for _, value := range values {
		if _, ok := value.(string); !ok {
			return false
		}
	}
	return true
}

func stripSchemaProse(value any) {
	switch current := value.(type) {
	case map[string]any:
		delete(current, "description")
		delete(current, "default") // annotation only; runtime defaults do not change
		if current["type"] == "string" {
			if values, ok := current["enum"].([]any); ok && len(values) > 0 && onlyStringEnum(values) {
				// The enum already excludes every non-string value. Removing
				// this redundant type does not widen the accepted input set.
				delete(current, "type")
			}
		}
		for key, child := range current {
			switch key {
			case "properties", "patternProperties", "$defs", "definitions", "dependentSchemas":
				if properties, ok := child.(map[string]any); ok {
					for _, property := range properties {
						stripSchemaProse(property)
					}
				}
			case "items", "prefixItems", "additionalItems", "additionalProperties", "unevaluatedProperties", "unevaluatedItems", "contains", "propertyNames", "not", "if", "then", "else", "allOf", "anyOf", "oneOf", "contentSchema":
				stripSchemaProse(child)
			}
			// Never recurse into const/enum/examples or unknown data: their
			// keys, including "description" and "default", may be literal data.
		}
	case []any:
		for _, child := range current {
			stripSchemaProse(child)
		}
	}
}
