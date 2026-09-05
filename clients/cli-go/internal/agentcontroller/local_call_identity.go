package agentcontroller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
)

const localCallPrefix = "local_call_"

// These authenticated immutable markers outlive a consumed WAL and are
// independent of best-effort output collection. They contain no executable
// arguments, raw call identifier, task state, input or process identity.
func localCallMarker(callID string) (string, []byte) {
	digest := sha256.Sum256([]byte(callID))
	encoded := hex.EncodeToString(digest[:])
	return localCallPrefix + encoded, []byte("local-call-v1\n" + encoded)
}

func validateLocalCallMarker(data, expected []byte) error {
	if bytes.Equal(data, expected) {
		return nil
	}
	if bytes.HasPrefix(data, []byte("local-call-v")) && !bytes.HasPrefix(data, []byte("local-call-v1\n")) {
		return agentsession.ErrVersionUnsupported
	}
	return agentsession.ErrCorrupt
}

func (c *Controller) checkLocalCallIdentityLocked(ctx context.Context, callID string) error {
	name, expected := localCallMarker(callID)
	data, err := c.handle.ReadArtifact(ctx, name)
	if errors.Is(err, agentsession.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := validateLocalCallMarker(data, expected); err != nil {
		return err
	}
	return agentloop.ErrLocalCallRecorded
}

// A checkpoint may consume local intents only after all their identity markers
// are confirmed durable. Partial/uncertain publication keeps the WAL intact;
// a later recovery can authenticate an already published marker without retrying
// execution or overwriting identity evidence.
func (c *Controller) preserveLocalCallIdentitiesLocked(ctx context.Context, intents []agentsession.LocalEffectIntent) error {
	for _, intent := range intents {
		name, expected := localCallMarker(intent.ToolCallID)
		data, err := c.handle.ReadArtifact(ctx, name)
		if err == nil {
			if err := validateLocalCallMarker(data, expected); err != nil {
				return err
			}
			continue
		}
		if !errors.Is(err, agentsession.ErrNotFound) {
			return err
		}
		if err := c.handle.WriteArtifact(ctx, name, expected); err != nil {
			return err
		}
	}
	return nil
}
