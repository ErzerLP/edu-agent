package main

import (
	"net/http"
	"testing"
)

func TestNotebookFixtureRequiresIsolatedSchema(t *testing.T) {
	for _, name := range []string{"public", "edu_agent", "web_candidate_nothex", ""} {
		if requireBrowserSchema(name) == nil {
			t.Fatal("unsafe fixture schema", name)
		}
	}
	if err := requireBrowserSchema("web_candidate_0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
}

func TestNotebookSearchFixtureRejectsUntrustedDestinations(t *testing.T) {
	transport := notebookSearchTransport{upstream: "http://127.0.0.1:1"}
	for _, destination := range []string{"http://api.search.brave.com/res/v1/web/search", "https://outside.example/res/v1/web/search", "https://api.search.brave.com/private"} {
		request, _ := http.NewRequest("GET", destination, nil)
		if _, err := transport.RoundTrip(request); err == nil || err.Error() != "fixture search destination denied" {
			t.Fatal("fixture accepted wrong destination", err)
		}
	}
}
