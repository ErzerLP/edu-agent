package knowledge

import (
	"strings"
	"testing"
)

const (
	testDocumentID = "11111111-1111-4111-8111-111111111111"
	testRootID     = "22222222-2222-4222-8222-222222222222"
	testNodeOneID  = "33333333-3333-4333-8333-333333333333"
	testNodeTwoID  = "44444444-4444-4444-8444-444444444444"
)

func TestCanonicalizerBuildsDeterministicTreeAndRanges(t *testing.T) {
	canonicalizer := NewCanonicalizer()
	input := "\ufeff---\r\ntitle: Demo\r\n---\r\nPreamble\r\n\r\nTop\r\n===\r\nBody\r\n\r\n### Child\r\nChild body"
	inspected, err := canonicalizer.Inspect(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspected.DraftNodes) != 2 || inspected.DraftNodes[0].Level != 1 || inspected.DraftNodes[1].Level != 3 {
		t.Fatalf("unexpected draft headings: %+v", inspected.DraftNodes)
	}
	first, err := canonicalizer.Materialize(inspected, testDocumentID, testRootID, []string{testNodeOneID, testNodeTwoID})
	if err != nil {
		t.Fatal(err)
	}
	second, err := canonicalizer.Index(first.CanonicalMarkdown)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.CanonicalHash != second.CanonicalHash || len(first.Nodes) != 3 {
		t.Fatalf("rebuild was not deterministic: first=%+v second=%+v", first, second)
	}
	if first.Nodes[2].ParentNodeRevisionID == nil || *first.Nodes[2].ParentNodeRevisionID != first.Nodes[1].ID {
		t.Fatalf("heading level jump did not attach to nearest lower heading: %+v", first.Nodes)
	}
	if first.Nodes[1].AncestorTitles == nil || len(first.Nodes[1].AncestorTitles) != 0 {
		t.Fatalf("top-level ancestor titles must encode as an empty array: %+v", first.Nodes[1].AncestorTitles)
	}
	for _, node := range first.Nodes {
		for _, sourceRange := range []SourceRange{node.HeadingRange, node.LocalBodyRange, node.SectionRange} {
			if sourceRange.Start < 0 || sourceRange.End < sourceRange.Start || sourceRange.End > len(first.CanonicalMarkdown) {
				t.Fatalf("invalid source range: %+v", sourceRange)
			}
		}
	}
	headingSlice := first.CanonicalMarkdown[first.Nodes[1].HeadingRange.Start:first.Nodes[1].HeadingRange.End]
	if headingSlice != "Top\n===\n" {
		t.Fatalf("setext heading range = %q", headingSlice)
	}
	if !strings.Contains(first.CanonicalMarkdown, "title: Demo\n") || strings.Contains(first.CanonicalMarkdown, "\r") {
		t.Fatalf("user YAML or LF normalization was not preserved: %q", first.CanonicalMarkdown)
	}
}

func TestCanonicalizerMarkerBoundariesAndExportRoundTrip(t *testing.T) {
	canonicalizer := NewCanonicalizer()
	pseudo := "```md\n<!-- edu-agent-node:v1 {\"id\":\"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa\"} -->\n```\n\n> <!-- edu-agent-node:v1 {\"id\":\"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb\"} -->\n\n# Real\nbody\n"
	inspected, err := canonicalizer.Inspect(pseudo)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspected.DraftNodes) != 1 || inspected.DraftNodes[0].ExplicitNodeID != "" {
		t.Fatalf("pseudo markers were recognized: %+v", inspected.DraftNodes)
	}
	document, err := canonicalizer.Materialize(inspected, testDocumentID, testRootID, []string{testNodeOneID})
	if err != nil {
		t.Fatal(err)
	}
	exported, err := ExportMarkdown(document.CanonicalMarkdown, "55555555-5555-4555-8555-555555555555")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(exported, "edu-agent-source-revision-id") != 1 || !strings.Contains(exported, pseudo[:20]) {
		t.Fatalf("invalid export view: %q", exported)
	}
	reimported, err := canonicalizer.Inspect(exported)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := canonicalizer.Materialize(reimported, testDocumentID, testRootID, []string{testNodeOneID})
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.CanonicalHash != document.CanonicalHash {
		t.Fatalf("source revision metadata affected canonical hash\nfirst=%q\nsecond=%q", document.CanonicalMarkdown, rebuilt.CanonicalMarkdown)
	}
}

func TestCanonicalizerPreservesFlowStyleFrontMatter(t *testing.T) {
	canonicalizer := NewCanonicalizer()
	userPairs := `  tags : ["go, db", { nested: 'yes, still' }], alias : "demo, quoted" , list: [one, two] `
	input := "---\n{" + userPairs + "}\n---\n# Topic\nbody\n"
	inspected, err := canonicalizer.Inspect(input)
	if err != nil {
		t.Fatal(err)
	}
	if !inspected.UserFrontMatterFlow {
		t.Fatal("flow-style frontmatter was not retained as flow style")
	}
	document, err := canonicalizer.Materialize(inspected, testDocumentID, testRootID, []string{testNodeOneID})
	if err != nil {
		t.Fatal(err)
	}
	front, _, err := parseFrontMatter([]byte(document.CanonicalMarkdown))
	if err != nil || front.user != userPairs {
		t.Fatalf("flow-style user pair bytes changed: got=%q want=%q err=%v", front.user, userPairs, err)
	}
	if !strings.Contains(document.CanonicalMarkdown, userPairs) {
		t.Fatalf("flow-style user pair bytes were not materialized verbatim: %q", document.CanonicalMarkdown)
	}
	exported, err := ExportMarkdown(document.CanonicalMarkdown, "55555555-5555-4555-8555-555555555555")
	if err != nil {
		t.Fatal(err)
	}
	reimported, err := canonicalizer.Inspect(exported)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := canonicalizer.Materialize(reimported, testDocumentID, testRootID, []string{testNodeOneID})
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.CanonicalHash != document.CanonicalHash || !strings.Contains(rebuilt.CanonicalMarkdown, userPairs) {
		t.Fatalf("flow-style export/reimport changed canonical identity or user bytes\nfirst=%q\nsecond=%q", document.CanonicalMarkdown, rebuilt.CanonicalMarkdown)
	}
}

func TestCanonicalizerPreservesFlowStyleComments(t *testing.T) {
	input := "---\n{note: value # comma, in comment\n}\n---\n# Topic\nbody\n"
	inspected, err := NewCanonicalizer().Inspect(input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(inspected.UserFrontMatter, "# comma, in comment") {
		t.Fatalf("flow comment was not preserved: %q", inspected.UserFrontMatter)
	}
	document, err := NewCanonicalizer().Materialize(inspected, testDocumentID, testRootID, []string{testNodeOneID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(document.CanonicalMarkdown, "# comma, in comment") {
		t.Fatalf("flow comment was not materialized verbatim: %q", document.CanonicalMarkdown)
	}
}

func TestCanonicalizerRejectsUnsafeFlowMappingAtomically(t *testing.T) {
	input := "---\n{tags: [one, {nested: two}\n---\n# Topic\nbody\n"
	if _, err := NewCanonicalizer().Inspect(input); ErrorCode(err) != CodeInvalidMarkdown {
		t.Fatalf("unsafe flow mapping was accepted: %v", err)
	}
}

func TestCanonicalizerRejectsNUL(t *testing.T) {
	if _, err := NewCanonicalizer().Inspect("before\x00after"); ErrorCode(err) != CodeInvalidMarkdown {
		t.Fatalf("NUL markdown error = %v", err)
	}
}

func TestCanonicalizerRejectsDuplicateAndTrailingMarkerJSON(t *testing.T) {
	canonicalizer := NewCanonicalizer()
	validID := "33333333-3333-4333-8333-333333333333"
	otherID := "44444444-4444-4444-8444-444444444444"
	inputs := []string{
		"<!-- edu-agent-node:v1 {\"id\":\"" + validID + "\",\"id\":\"" + otherID + "\"} -->\n# Topic\n",
		"<!-- edu-agent-node:v1 {\"id\":\"" + validID + "\"} {\"id\":\"" + otherID + "\"} -->\n# Topic\n",
	}
	for _, input := range inputs {
		if _, err := canonicalizer.Inspect(input); ErrorCode(err) != CodeInvalidIdentityMarker {
			t.Fatalf("invalid marker JSON was accepted: input=%q err=%v", input, err)
		}
	}
}

func TestCanonicalizerRejectsInvalidEnvelopeAndMarkers(t *testing.T) {
	canonicalizer := NewCanonicalizer()
	cases := []string{
		"---\nedu-agent-format: 1\nedu-agent-format: 1\n---\n# H\n",
		"<!-- edu-agent-node:v1 {\"id\":\"not-uuid\"} -->\n# H\n",
		"<!-- edu-agent-node:v1 {\"id\":\"33333333-3333-4333-8333-333333333333\"} -->\norphan\n",
		"<!-- edu-agent-node:v1 {\"id\":\"33333333-3333-4333-8333-333333333333\"} -->\n# A\n\n<!-- edu-agent-node:v1 {\"id\":\"33333333-3333-4333-8333-333333333333\"} -->\n# B\n",
	}
	for _, input := range cases {
		if _, err := canonicalizer.Inspect(input); err == nil {
			t.Fatalf("expected invalid identity input to fail: %q", input)
		}
	}
}
