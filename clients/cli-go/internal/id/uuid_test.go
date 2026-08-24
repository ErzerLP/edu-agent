package id

import (
	"regexp"
	"testing"
)

func TestNewUUIDReturnsCanonicalLowercaseV4(t *testing.T) {
	t.Parallel()
	value, err := NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !pattern.MatchString(value) {
		t.Fatalf("UUID = %q", value)
	}
}
