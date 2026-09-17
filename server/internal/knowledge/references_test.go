package knowledge

import (
	"github.com/google/uuid"
	"reflect"
	"testing"
)

func TestReferenceScopeFiltersBeforeRetrieval(t *testing.T) {
	base := ScopeEntry{CollectionID: uuid.NewString(), RevisionID: uuid.NewString()}
	selected := ScopeEntry{CollectionID: uuid.NewString(), RevisionID: uuid.NewString(), DocumentID: uuid.NewString(), NodeID: uuid.NewString()}
	for _, role := range []string{"supplement", "prefer", "restrict"} {
		s := ReferenceSelection{Entries: []ReferenceEntry{{ScopeEntry: selected, Role: role}}}
		if err := s.Validate(); err != nil {
			t.Fatal(err)
		}
		got := ReferenceScope([]ScopeEntry{base}, s.Entries)
		want := []ScopeEntry{base, selected}
		if role == "restrict" {
			want = []ScopeEntry{selected}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("角色 %s 范围不符：%+v", role, got)
		}
	}
	if (ReferenceSelection{Entries: []ReferenceEntry{{ScopeEntry: selected, Role: "automatic"}}}).Validate() == nil {
		t.Fatal("接受未知角色")
	}
	if (ReferenceSelection{SessionID: "其他目标", Entries: []ReferenceEntry{}}).Validate() == nil {
		t.Fatal("接受非法作用目标")
	}
}
