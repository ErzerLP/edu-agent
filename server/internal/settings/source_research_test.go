package settings

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestSourceResearchScalePersistsAndRejectsInvalidInput(t *testing.T) {
	s, options := testService(t)
	raw, _ := json.Marshal(s.View())
	var view map[string]json.RawMessage
	_ = json.Unmarshal(raw, &view)
	if string(view["source_research"]) != `{"max_sources_per_round":10,"max_pages_per_round":30}` {
		t.Fatalf("missing default scale: %s", view["source_research"])
	}
	for _, input := range []string{
		`{"max_sources_per_round":0,"max_pages_per_round":30}`,
		`{"max_sources_per_round":21,"max_pages_per_round":30}`,
		`{"max_sources_per_round":3,"max_pages_per_round":2}`,
		`{"max_sources_per_round":3,"max_pages_per_round":101}`,
	} {
		var u Update
		err := json.Unmarshal([]byte(`{"expected_revision":0,"source_research":`+input+`}`), &u)
		if err == nil {
			_, err = s.Update(u)
		}
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid scale not rejected: %v", err)
		}
	}
	var u Update
	if err := json.Unmarshal([]byte(`{"expected_revision":0,"source_research":{"max_sources_per_round":3,"max_pages_per_round":8}}`), &u); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(u); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(options)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(reopened.View())
	_ = json.Unmarshal(raw, &view)
	if string(view["source_research"]) != `{"max_sources_per_round":3,"max_pages_per_round":8}` {
		t.Fatal("scale was not persisted")
	}
	if _, err = reopened.Update(u); !errors.Is(err, ErrConflict) {
		t.Fatal("stale settings revision accepted")
	}
}
