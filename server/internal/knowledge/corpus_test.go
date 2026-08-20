package knowledge

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestCanonicalGoldenCorpusPreservesMarkdownAndSlices(t *testing.T) {
	type nodeSlices struct {
		heading string
		local   string
		section string
	}
	corpus := []struct {
		name          string
		markdown      string
		canonicalBody string
		nodes         []nodeSlices
	}{
		{
			name: "ATX", markdown: "# ATX\nbody\n", canonicalBody: "{{M0}}# ATX\nbody\n",
			nodes: []nodeSlices{{heading: "# ATX\n", local: "body\n", section: "{{M0}}# ATX\nbody\n"}},
		},
		{
			name: "Setext", markdown: "Setext\n===\nbody\n", canonicalBody: "{{M0}}Setext\n===\nbody\n",
			nodes: []nodeSlices{{heading: "Setext\n===\n", local: "body\n", section: "{{M0}}Setext\n===\nbody\n"}},
		},
		{
			name: "heading level jump", markdown: "# Top\nintro\n\n### Child\nchild\n", canonicalBody: "{{M0}}# Top\nintro\n\n{{M1}}### Child\nchild\n",
			nodes: []nodeSlices{
				{heading: "# Top\n", local: "intro\n\n", section: "{{M0}}# Top\nintro\n\n{{M1}}### Child\nchild\n"},
				{heading: "### Child\n", local: "child\n", section: "{{M1}}### Child\nchild\n"},
			},
		},
		{
			name: "duplicate headings", markdown: "# Same\nfirst\n\n# Same\nsecond\n", canonicalBody: "{{M0}}# Same\nfirst\n\n{{M1}}# Same\nsecond\n",
			nodes: []nodeSlices{
				{heading: "# Same\n", local: "first\n\n", section: "{{M0}}# Same\nfirst\n\n"},
				{heading: "# Same\n", local: "second\n", section: "{{M1}}# Same\nsecond\n"},
			},
		},
		{name: "no heading", markdown: "plain document without a trailing newline", canonicalBody: "plain document without a trailing newline"},
		{
			name: "existing YAML", markdown: "---\ntags: [go, db]\nalias: demo\n---\n# YAML\nbody\n", canonicalBody: "{{M0}}# YAML\nbody\n",
			nodes: []nodeSlices{{heading: "# YAML\n", local: "body\n", section: "{{M0}}# YAML\nbody\n"}},
		},
		{
			name: "fenced pseudo heading and marker", markdown: "```md\n# Fake\n<!-- edu-agent-node:v1 {\"id\":\"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa\"} -->\n```\n\n# Real\nbody\n",
			canonicalBody: "```md\n# Fake\n<!-- edu-agent-node:v1 {\"id\":\"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa\"} -->\n```\n\n{{M0}}# Real\nbody\n",
			nodes:         []nodeSlices{{heading: "# Real\n", local: "body\n", section: "{{M0}}# Real\nbody\n"}},
		},
		{
			name: "HTML", markdown: "<section>raw html</section>\n\n# Heading\nbody\n", canonicalBody: "<section>raw html</section>\n\n{{M0}}# Heading\nbody\n",
			nodes: []nodeSlices{{heading: "# Heading\n", local: "body\n", section: "{{M0}}# Heading\nbody\n"}},
		},
		{
			name: "GFM table and task", markdown: "# GFM\n\n| A | B |\n|---|---|\n| 1 | 2 |\n\n- [x] done\n", canonicalBody: "{{M0}}# GFM\n\n| A | B |\n|---|---|\n| 1 | 2 |\n\n- [x] done\n",
			nodes: []nodeSlices{{heading: "# GFM\n", local: "\n| A | B |\n|---|---|\n| 1 | 2 |\n\n- [x] done\n", section: "{{M0}}# GFM\n\n| A | B |\n|---|---|\n| 1 | 2 |\n\n- [x] done\n"}},
		},
		{
			name: "wiki link", markdown: "# Wiki\n[[Target|Alias]]\n", canonicalBody: "{{M0}}# Wiki\n[[Target|Alias]]\n",
			nodes: []nodeSlices{{heading: "# Wiki\n", local: "[[Target|Alias]]\n", section: "{{M0}}# Wiki\n[[Target|Alias]]\n"}},
		},
		{
			name: "callout", markdown: "# Callout\n> [!NOTE]\n> body\n", canonicalBody: "{{M0}}# Callout\n> [!NOTE]\n> body\n",
			nodes: []nodeSlices{{heading: "# Callout\n", local: "> [!NOTE]\n> body\n", section: "{{M0}}# Callout\n> [!NOTE]\n> body\n"}},
		},
		{
			name: "CJK", markdown: "# 并发\n通道传递消息\n", canonicalBody: "{{M0}}# 并发\n通道传递消息\n",
			nodes: []nodeSlices{{heading: "# 并发\n", local: "通道传递消息\n", section: "{{M0}}# 并发\n通道传递消息\n"}},
		},
		{
			name: "BOM", markdown: "\ufeff# BOM\nbody\n", canonicalBody: "{{M0}}# BOM\nbody\n",
			nodes: []nodeSlices{{heading: "# BOM\n", local: "body\n", section: "{{M0}}# BOM\nbody\n"}},
		},
		{
			name: "CRLF", markdown: "# Windows\r\nline one\r\nline two\r\n", canonicalBody: "{{M0}}# Windows\nline one\nline two\n",
			nodes: []nodeSlices{{heading: "# Windows\n", local: "line one\nline two\n", section: "{{M0}}# Windows\nline one\nline two\n"}},
		},
		{
			name: "no trailing newline", markdown: "# Final\nlast line", canonicalBody: "{{M0}}# Final\nlast line",
			nodes: []nodeSlices{{heading: "# Final\n", local: "last line", section: "{{M0}}# Final\nlast line"}},
		},
	}
	canonicalizer := NewCanonicalizer()
	for _, fixture := range corpus {
		t.Run(fixture.name, func(t *testing.T) {
			inspected, err := canonicalizer.Inspect(fixture.markdown)
			if err != nil {
				t.Fatal(err)
			}
			if len(inspected.DraftNodes) != len(fixture.nodes) {
				t.Fatalf("heading count = %d, want %d", len(inspected.DraftNodes), len(fixture.nodes))
			}
			nodeIDs := make([]string, len(fixture.nodes))
			for i := range nodeIDs {
				nodeIDs[i] = uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("%s-%d", fixture.name, i))).String()
			}
			document, err := canonicalizer.Materialize(
				inspected,
				uuid.NewSHA1(uuid.NameSpaceURL, []byte("document-"+fixture.name)).String(),
				uuid.NewSHA1(uuid.NameSpaceURL, []byte("root-"+fixture.name)).String(),
				nodeIDs,
			)
			if err != nil {
				t.Fatal(err)
			}
			expand := func(value string) string {
				for i, id := range nodeIDs {
					marker := fmt.Sprintf("<!-- edu-agent-node:v1 {\"id\":\"%s\"} -->\n", id)
					value = strings.ReplaceAll(value, fmt.Sprintf("{{M%d}}", i), marker)
				}
				return value
			}
			_, canonicalBody, err := parseFrontMatter([]byte(document.CanonicalMarkdown))
			if err != nil {
				t.Fatal(err)
			}
			expectedBody := expand(fixture.canonicalBody)
			if canonicalBody != expectedBody {
				t.Fatalf("canonical body differs byte-for-byte:\n got %q\nwant %q", canonicalBody, expectedBody)
			}
			root := document.Nodes[0]
			firstMarker := -1
			if len(nodeIDs) != 0 {
				firstMarker = strings.Index(expectedBody, fmt.Sprintf("<!-- edu-agent-node:v1 {\"id\":\"%s\"} -->\n", nodeIDs[0]))
			}
			expectedRootLocal := expectedBody
			if firstMarker >= 0 {
				expectedRootLocal = expectedBody[:firstMarker]
			}
			if got := document.CanonicalMarkdown[root.LocalBodyRange.Start:root.LocalBodyRange.End]; got != expectedRootLocal {
				t.Fatalf("root local slice = %q, want %q", got, expectedRootLocal)
			}
			if got := document.CanonicalMarkdown[root.SectionRange.Start:root.SectionRange.End]; got != expectedBody {
				t.Fatalf("root section slice = %q, want %q", got, expectedBody)
			}
			for i, expected := range fixture.nodes {
				node := document.Nodes[i+1]
				if got, want := document.CanonicalMarkdown[node.HeadingRange.Start:node.HeadingRange.End], expand(expected.heading); got != want {
					t.Fatalf("node %d heading slice = %q, want %q", i, got, want)
				}
				if got, want := document.CanonicalMarkdown[node.LocalBodyRange.Start:node.LocalBodyRange.End], expand(expected.local); got != want {
					t.Fatalf("node %d local slice = %q, want %q", i, got, want)
				}
				if got, want := document.CanonicalMarkdown[node.SectionRange.Start:node.SectionRange.End], expand(expected.section); got != want {
					t.Fatalf("node %d section slice = %q, want %q", i, got, want)
				}
			}
		})
	}
}

func TestPathNormalizationAndFoldedCollision(t *testing.T) {
	valid, err := NormalizePath("课程/并发.md")
	if err != nil || valid != "课程/并发.md" {
		t.Fatalf("valid Unicode path: path=%q err=%v", valid, err)
	}
	for _, invalid := range []string{"/absolute.md", "a//b.md", "a/../b.md", "a/./b.md", `a\b.md`, "a\x00b.md", "a\nb.md"} {
		if _, err := NormalizePath(invalid); ErrorCode(err) != CodeInvalidPath {
			t.Fatalf("path %q was accepted: %v", invalid, err)
		}
	}
	service, _ := testKnowledgeService(t)
	_, err = service.Import(t.Context(), ImportCommand{
		OperationID: "10000000-0000-4000-8000-000000000041", ExpectedParentProvided: true,
		Source: "paths", ActorDeviceID: "90000000-0000-4000-8000-000000000001",
		Documents: []ImportDocument{
			{Path: "Résumé.md", Markdown: "first"},
			{Path: "re\u0301sume\u0301.MD", Markdown: "second"},
		},
	})
	if ErrorCode(err) != CodeInvalidPath {
		t.Fatalf("NFC + case-fold collision was accepted: %v", err)
	}
}
