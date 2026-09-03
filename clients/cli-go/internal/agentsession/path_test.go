package agentsession

import "testing"

func TestProfileFingerprintUsesNormalizedOriginOnly(t *testing.T) {
	first, err := ProfileFingerprint("https://EXAMPLE.test:443/api/v1?x=1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ProfileFingerprint("https://example.test/other")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("same origin fingerprints differ: %q %q", first, second)
	}
	other, err := ProfileFingerprint("https://other.example.test/")
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Fatal("different origins share a fingerprint")
	}
}
