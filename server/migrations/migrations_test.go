package migrations

import "testing"

func TestEmbeddedMigrationsAreOrderedAndUnique(t *testing.T) {
	items, err := load()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(items); i++ {
		if items[i-1].version >= items[i].version {
			t.Fatalf("migrations are not strictly ordered: %+v", items)
		}
	}
	if items[0].checksum == "" || len(items[0].body) == 0 {
		t.Fatal("migration checksum or body is empty")
	}
}
