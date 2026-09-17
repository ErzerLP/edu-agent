package mentorrun

import (
	"context"
	"encoding/json"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
)

func (h *executionHost) readReferences(ctx context.Context) (string, error) {
	if !h.service.changes.ReferencesAvailable() {
		return "当前服务器尚未配置参考采用能力。", nil
	}
	scoped, err := learningspace.WithScope(ctx, h.owned.SpaceID)
	if err != nil {
		return "", err
	}
	actor := identity.Credential{Device: identity.Device{ID: h.owned.device}, TokenID: h.owned.token}
	value, err := h.service.changes.ReferenceMaterial(scoped, actor, h.owned.GoalID, h.owned.TeachingSessionID)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(value)
	return string(raw), err
}
