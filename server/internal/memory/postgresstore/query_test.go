package postgresstore

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/google/uuid"
)

func TestPageCursorBindsLearnerAndMemoryGeneration(t *testing.T) {
	generation := memory.Generation{LearnerGeneration: 7, MemoryGeneration: 7}
	value := pageCursor{
		LearnerGeneration: generation.LearnerGeneration,
		MemoryGeneration:  generation.MemoryGeneration,
		Time:              time.Date(2026, time.September, 4, 5, 6, 7, 8, time.UTC),
		ID:                uuid.NewString(),
	}
	encoded := encodePageCursor(value)
	decoded, err := decodePageCursor(encoded, generation)
	if err != nil || decoded.LearnerGeneration != 7 || decoded.MemoryGeneration != 7 || decoded.ID != value.ID || !decoded.Time.Equal(value.Time) {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
	for _, stale := range []memory.Generation{
		{LearnerGeneration: 8, MemoryGeneration: 7},
		{LearnerGeneration: 7, MemoryGeneration: 8},
	} {
		if _, err := decodePageCursor(encoded, stale); memory.ErrorCode(err) != memory.CodeStaleCursor {
			t.Fatalf("generation=%+v err=%v", stale, err)
		}
	}
}

func TestPageCursorRejectsLegacyWireFormat(t *testing.T) {
	legacy, err := json.Marshal(map[string]any{
		"time": time.Date(2026, time.September, 4, 5, 6, 7, 0, time.UTC).Format(time.RFC3339Nano),
		"id":   uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(legacy)
	if _, err := decodePageCursor(encoded, memory.Generation{LearnerGeneration: 1, MemoryGeneration: 1}); memory.ErrorCode(err) != memory.CodeStaleCursor {
		t.Fatalf("legacy cursor err=%v", err)
	}
}
