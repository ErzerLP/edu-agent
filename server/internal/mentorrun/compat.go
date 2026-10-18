package mentorrun

import (
	"bytes"
	"encoding/json"
)

// Deprecated budgets are optional nonnegative integers. Keep their typed values
// for receipt hashing; never turn them into execution allowances.
func decodeCompatibleRequest(raw []byte, target any) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, name := range []string{"request_budget", "token_budget"} {
		if value, ok := fields[name]; ok {
			var number int
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &number) != nil || number < 0 {
				return ErrInvalid
			}
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(target)
}

func (c *Create) UnmarshalJSON(raw []byte) error {
	type plain Create
	return decodeCompatibleRequest(raw, (*plain)(c))
}
func (c *Command) UnmarshalJSON(raw []byte) error {
	type plain Command
	return decodeCompatibleRequest(raw, (*plain)(c))
}
func (c *SubmitTurn) UnmarshalJSON(raw []byte) error {
	type plain SubmitTurn
	return decodeCompatibleRequest(raw, (*plain)(c))
}
func (c *StudioGenerate) UnmarshalJSON(raw []byte) error {
	type plain StudioGenerate
	return decodeCompatibleRequest(raw, (*plain)(c))
}
func (c *StudyAssessmentResume) UnmarshalJSON(raw []byte) error {
	type plain StudyAssessmentResume
	return decodeCompatibleRequest(raw, (*plain)(c))
}
