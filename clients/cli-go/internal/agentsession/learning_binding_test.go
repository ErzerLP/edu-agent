package agentsession

import (
	"encoding/json"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentcontext"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

func withoutLearningBindingForTest(t *testing.T, raw []byte) []byte {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "learning_binding")
	result, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestLearningBindingEncryptedRoundTripAndLegacyDefault(t *testing.T) {
	s := openTestStore(t, t.TempDir(), &memorySecretBackend{}, Limits{})
	binding := agentcontext.Binding{SpaceID: "10000000-0000-4000-8000-000000000002", GoalID: "20000000-0000-4000-8000-000000000002"}
	h, record, err := s.Create(t.Context(), CreateInput{Title: "绑定测试", LearningBinding: binding, Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer h.Close()
	saved, err := h.Save(t.Context(), record.RecordRevision, record)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := h.Load()
	if err != nil || loaded.Record.LearningBinding != record.LearningBinding {
		t.Fatalf("绑定未保留：%+v %v", loaded.Record.LearningBinding, err)
	}
	items, err := s.List(t.Context())
	if err != nil || len(items) != 1 || items[0].LearningBinding != record.LearningBinding {
		t.Fatalf("索引绑定错误：%+v %v", items, err)
	}
	writeRecordPayloadForTest(t, s, h.dataKey, saved, 9, nil)
	loaded, err = h.Load()
	if err != nil || loaded.Record.LearningBinding.SpaceID != api.DefaultLearningSpaceID || loaded.Record.LearningBinding.GoalID != "" {
		t.Fatalf("旧记录必须固定默认区：%+v %v", loaded.Record.LearningBinding, err)
	}
}
