package postgresstore

import (
	"bytes"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
)

func TestValidateCanonicalRedactionEventUpcastsLegacySchema(t *testing.T) {
	const (
		erasureID = "10000000-0000-4000-8000-000000000001"
		eventID   = "20000000-0000-4000-8000-000000000001"
	)
	request := privacy.LocalRedactionRequest{
		ErasureID:            erasureID,
		LearnerGeneration:    2,
		RedactedThroughEvent: 41,
	}
	event := learning.LearningEvent{
		ID:               eventID,
		EventSequence:    42,
		Type:             learning.EventRedacted,
		SchemaVersion:    learning.EventSchemaVersion,
		AggregateType:    "privacy",
		AggregateID:      erasureID,
		AggregateVersion: 1,
		Payload:          []byte(`{"erasure_id":"10000000-0000-4000-8000-000000000001","generation":2,"redacted_through":41,"policy_version":"privacy-redaction-v1","reason_code":"learner_request"}`),
	}
	decoded, err := validateCanonicalRedactionEvent(event, request, eventID, "privacy-redaction-v1", "learner_request")
	if err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != learning.EventRedactedSchemaVersion {
		t.Fatalf("canonical replay schema version=%d", decoded.SchemaVersion)
	}
	if bytes.Contains(decoded.Payload, []byte(`"redacted_through"`)) || !bytes.Contains(decoded.Payload, []byte(`"redacted_through_event_seq":41`)) {
		t.Fatalf("legacy payload was not canonically upcast: %s", decoded.Payload)
	}
	if _, err := learning.NewEventRegistry().Decode(decoded); err != nil {
		t.Fatalf("canonical event could not be decoded again by replay: %v", err)
	}
}
