package research

import "testing"

func TestNormalizeURLKeepsContentQuery(t *testing.T) {
	got, err := NormalizeURL("https://Example.org:443/book?chapter=2&utm_source=news#page=4")
	if err != nil || got != "https://example.org/book?chapter=2" {
		t.Fatalf("URL identity: %q %v", got, err)
	}
	got, err = NormalizeURL("https://example.org/doc?z=a%20b&a=2&utm_source=news")
	if err != nil || got != "https://example.org/doc?z=a%20b&a=2" {
		t.Fatalf("semantic/signed query bytes changed: %q %v", got, err)
	}
}
