package fixture

import (
	"encoding/json"
	"fmt"
	"reflect"
)

const (
	capabilitySystemPrompt    = "Return only JSON matching the requested schema."
	capabilityAssistantPrompt = "I will return the requested JSON object."
	capabilityUserPrompt      = "Confirm the core profile."
	proposalSystemPrompt      = "Return only a JSON object matching the requested tutoring proposal schema. Never create canonical IDs, state, evidence, mastery, or dates."
	proposalAssistantPrompt   = "I will return only the requested proposal JSON."
)

const capabilitySchemaContract = `{
	"type":"object",
	"properties":{"capability_probe":{"type":"boolean"}},
	"required":["capability_probe"],
	"additionalProperties":false
}`

const routeSchemaContract = `{
	"type":"object",
	"properties":{"route":{"type":"array","items":{
		"type":"object",
		"properties":{
			"node_revision_id":{"type":"string"},
			"teaching_intent":{"type":"string"},
			"completion_condition":{"type":"string"}
		},
		"required":["node_revision_id","teaching_intent","completion_condition"],
		"additionalProperties":false
	}}},
	"required":["route"],
	"additionalProperties":false
}`

const activitySchemaContract = `{
	"type":"object",
	"properties":{"activity":{
		"type":"object",
		"properties":{
			"prompt":{"type":"string"},
			"type":{"type":"string","enum":["objective","open"]},
			"rubric":{
				"type":"object",
				"properties":{
					"rubric_revision":{"type":"string"},
					"items":{"type":"array","items":{
						"type":"object",
						"properties":{
							"rubric_item_id":{"type":"string"},
							"criterion":{"type":"string"},
							"required_reference_ids":{"type":"array","items":{"type":"string"}}
						},
						"required":["rubric_item_id","criterion"],
						"additionalProperties":false
					}},
					"objective_rule":{
						"type":"object",
						"properties":{
							"accepted_answers":{"type":"array","items":{"type":"string"}},
							"case_sensitive":{"type":"boolean"},
							"trim_space":{"type":"boolean"}
						},
						"required":["accepted_answers","case_sensitive","trim_space"],
						"additionalProperties":false
					}
				},
				"required":["rubric_revision","items"],
				"additionalProperties":false
			},
			"difficulty":{"type":"integer"},
			"allowed_help":{"type":"array","items":{"type":"string","enum":["none","hint","scaffold","answer_revealed"]}},
			"knowledge_references":{"type":"array","minItems":1,"items":{
				"type":"object",
				"properties":{
					"node_revision_id":{"type":"string"},
					"slice_sha256":{"type":"string"},
					"range":{
						"type":"object",
						"properties":{"start":{"type":"integer"},"end":{"type":"integer"}},
						"required":["start","end"],
						"additionalProperties":false
					}
				},
				"required":["node_revision_id"],
				"additionalProperties":false
			}}
		},
		"required":["prompt","type","rubric","difficulty","allowed_help","knowledge_references"],
		"additionalProperties":false
	}},
	"required":["activity"],
	"additionalProperties":false
}`

const assessmentSchemaContract = `{
	"type":"object",
	"properties":{"assessment":{
		"type":"object",
		"properties":{
			"items":{"type":"array","items":{
				"type":"object",
				"properties":{
					"rubric_item_id":{"type":"string"},
					"conclusion":{"type":"string","enum":["pass","partial","fail","unassessed"]},
					"answer_quote":{"type":"string"},
					"answer_range":{"type":"object","properties":{"start":{"type":"integer"},"end":{"type":"integer"}},"required":["start","end"],"additionalProperties":false},
					"answer_quote_sha256":{"type":"string"},
					"knowledge_reference_id":{"type":"string"},
					"knowledge_quote":{"type":"string"},
					"knowledge_range":{"type":"object","properties":{"start":{"type":"integer"},"end":{"type":"integer"}},"required":["start","end"],"additionalProperties":false},
					"knowledge_quote_sha256":{"type":"string"},
					"misconception_candidate":{"type":"string"}
				},
				"required":["rubric_item_id","conclusion","answer_quote","answer_range","answer_quote_sha256","knowledge_reference_id","knowledge_quote","knowledge_range","knowledge_quote_sha256"],
				"additionalProperties":false
			}},
			"rubric_complete":{"type":"boolean"},
			"confidence":{"type":"integer"},
			"risk_flags":{"type":"array","items":{"type":"string"}}
		},
		"required":["items","rubric_complete","confidence","risk_flags"],
		"additionalProperties":false
	}},
	"required":["assessment"],
	"additionalProperties":false
}`

const freeAnswerSchemaContract = `{
	"type":"object",
	"properties":{"text":{
		"type":"object",
		"properties":{
			"text":{"type":"string"},
			"knowledge_references":{"type":"array","minItems":1,"items":{
				"type":"object",
				"properties":{
					"node_revision_id":{"type":"string"},
					"slice_sha256":{"type":"string"},
					"range":{"type":"object","properties":{"start":{"type":"integer"},"end":{"type":"integer"}},"required":["start","end"],"additionalProperties":false}
				},
				"required":["node_revision_id"],
				"additionalProperties":false
			}}
		},
		"required":["text","knowledge_references"],
		"additionalProperties":false
	}},
	"required":["text"],
	"additionalProperties":false
}`

const explanationSchemaContract = freeAnswerSchemaContract

func validateCapabilityContract(request chatRequest) error {
	if !messagesMatch(request, capabilitySystemPrompt, capabilityAssistantPrompt, capabilityUserPrompt) {
		return fmt.Errorf("capability prompt contract mismatch")
	}
	if request.ResponseFormat.Type == "json_object" {
		if request.ResponseFormat.JSONSchema != nil {
			return fmt.Errorf("capability json_object must not include json_schema")
		}
		return nil
	}
	if request.ResponseFormat.Type != "json_schema" || request.ResponseFormat.JSONSchema == nil || request.ResponseFormat.JSONSchema.Name != "capability_probe" {
		return fmt.Errorf("capability response format contract mismatch")
	}
	return validateSchemaContract(request.ResponseFormat.JSONSchema.Schema, capabilitySchemaContract)
}

func validateProposalContract(request chatRequest, kind RequestKind) error {
	if len(request.Messages) != 3 || request.Messages[0].Role != "system" || request.Messages[0].Content != proposalSystemPrompt || request.Messages[1].Role != "assistant" || request.Messages[1].Content != proposalAssistantPrompt || request.Messages[2].Role != "user" {
		return fmt.Errorf("proposal prompt contract mismatch")
	}
	if request.ResponseFormat.Type != "json_schema" || request.ResponseFormat.JSONSchema == nil || request.ResponseFormat.JSONSchema.Name != "tutoring_proposal" {
		return fmt.Errorf("proposal response format contract mismatch")
	}
	expected, err := proposalSchemaContract(kind)
	if err != nil {
		return err
	}
	return validateSchemaContract(request.ResponseFormat.JSONSchema.Schema, expected)
}

func messagesMatch(request chatRequest, system, assistant, user string) bool {
	return len(request.Messages) == 3 &&
		request.Messages[0].Role == "system" && request.Messages[0].Content == system &&
		request.Messages[1].Role == "assistant" && request.Messages[1].Content == assistant &&
		request.Messages[2].Role == "user" && request.Messages[2].Content == user
}

func proposalSchemaContract(kind RequestKind) (string, error) {
	switch kind {
	case KindRoute:
		return routeSchemaContract, nil
	case KindActivity:
		return activitySchemaContract, nil
	case KindAssessment:
		return assessmentSchemaContract, nil
	case KindFreeAnswer:
		return freeAnswerSchemaContract, nil
	case KindExplanation:
		return explanationSchemaContract, nil
	default:
		return "", fmt.Errorf("unsupported proposal schema contract %q", kind)
	}
}

func validateSchemaContract(actual map[string]any, expectedJSON string) error {
	var expected map[string]any
	if err := json.Unmarshal([]byte(expectedJSON), &expected); err != nil {
		return fmt.Errorf("invalid frozen schema contract: %w", err)
	}
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("json schema contract mismatch")
	}
	return nil
}
