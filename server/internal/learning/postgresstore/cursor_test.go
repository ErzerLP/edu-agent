package postgresstore

import (
	"encoding/json"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
)

func TestCompositeCursorBindsGenerationAndCheckpoint(t *testing.T) {
	const checkpoint int64 = 41
	encoded := encodeCursor("evidence", "generation-a", checkpoint, "2026-08-20T12:00:00Z", "10000000-0000-4000-8000-000000000001")
	keys, err := decodeCursor(encoded, "evidence", "generation-a", checkpoint, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != "2026-08-20T12:00:00Z" || keys[1] != "10000000-0000-4000-8000-000000000001" {
		t.Fatalf("cursor keys=%v", keys)
	}
	if _, err := decodeCursor(encoded, "evidence", "generation-a", checkpoint+1, 2); learning.ErrorCode(err) != learning.CodeStaleCursor {
		t.Fatalf("checkpoint advance error=%v", err)
	}
	if _, err := decodeCursor(encoded, "evidence", "generation-b", checkpoint, 2); learning.ErrorCode(err) != learning.CodeStaleCursor {
		t.Fatalf("generation switch error=%v", err)
	}
	if _, err := decodeCursor(encoded, "evidence", "generation-a", checkpoint, 1); learning.ErrorCode(err) != learning.CodeStaleCursor {
		t.Fatalf("key shape error=%v", err)
	}
	keys, err = decodeCursor("", "evidence", "generation-a", checkpoint+1, 2)
	if err != nil || keys != nil {
		t.Fatalf("empty first-page cursor keys=%v err=%v", keys, err)
	}
}

func TestProjectionSwitchRequiresMatchingSeals(t *testing.T) {
	matching := projectionSeal{GenerationCheckpoint: 9, Checkpoint: 9, GenerationFingerprint: []byte{1, 2, 3}, CheckpointFingerprint: []byte{1, 2, 3}}
	if err := validateProjectionSwitch(9, matching, matching); err != nil {
		t.Fatal(err)
	}
	checkpointMismatch := matching
	checkpointMismatch.Checkpoint = 8
	if err := validateProjectionSwitch(9, matching, checkpointMismatch); learning.ErrorCode(err) != learning.CodeProjectionUnavailable {
		t.Fatalf("checkpoint mismatch err=%v", err)
	}
	fingerprintMismatch := matching
	fingerprintMismatch.GenerationFingerprint = []byte{9}
	fingerprintMismatch.CheckpointFingerprint = []byte{9}
	if err := validateProjectionSwitch(9, matching, fingerprintMismatch); learning.ErrorCode(err) != learning.CodeProjectionUnavailable {
		t.Fatalf("fingerprint mismatch err=%v", err)
	}
}

func TestFinalizeSessionResultUsesCommittedVersionAndFocusSequence(t *testing.T) {
	sessionID := "session"
	frame := &tutoring.FocusFrame{ID: "frame", SessionID: sessionID}
	session := tutoring.Session{ID: sessionID, State: tutoring.StateFreeQuestion, AggregateVer: 3, ActiveFrame: frame}
	batch := learning.CommandBatch{Session: &session, FocusFrame: frame, ResultSession: true}
	versions := map[aggregateKey]int64{{kind: "session", id: sessionID}: 7}
	if err := finalizeSessionResult(&batch, versions, 42); err != nil {
		t.Fatal(err)
	}
	var resultSession tutoring.Session
	if err := json.Unmarshal(batch.TypedResult, &resultSession); err != nil {
		t.Fatal(err)
	}
	if batch.Session.AggregateVer != 7 || resultSession.AggregateVer != 7 || batch.FocusFrame.CreatedEventSequence != 42 || resultSession.ActiveFrame == nil || resultSession.ActiveFrame.CreatedEventSequence != 42 {
		t.Fatalf("batch session=%+v frame=%+v result=%+v", batch.Session, batch.FocusFrame, resultSession)
	}
}
