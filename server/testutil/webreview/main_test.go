package main

import "testing"

func TestRequireBrowserSchema(t *testing.T) {
	for _, schema := range []string{"public", "edu_agent", "web_candidate_", "web_candidate_other", "web_candidate_0123456789abcdef0123456789abcdef,public"} {
		if err := requireBrowserSchema(schema); err == nil {
			t.Fatalf("允许在非隔离浏览器 schema %q 初始化历史证据", schema)
		}
	}
	if err := requireBrowserSchema("web_candidate_0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
}
