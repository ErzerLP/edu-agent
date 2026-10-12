package main

import "testing"

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
