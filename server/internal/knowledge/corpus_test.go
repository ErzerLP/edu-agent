package knowledge

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestCanonicalGoldenCorpusPreservesMarkdownAndSlices(t *testing.T) {
	corpus := []struct {
		name     string
		markdown string
		headings int
	}{
		{name: "no heading", markdown: "plain document without a trailing newline", headings: 0},
		{name: "duplicate ATX", markdown: "# Same\nfirst\n\n# Same\nsecond\n", headings: 2},
		{name: "GFM", markdown: "# GFM\n\n| A | B |\n|---|---|\n| 1 | 2 |\n\n- [x] done\n", headings: 1},
		{name: "Obsidian syntax", markdown: "# Notes\n[[Wiki Link]]\n\n> [!NOTE]\n> callout body\n", headings: 1},
		{name: "HTML", markdown: "<section>raw html</section>\n\n# Heading\nbody\n", headings: 1},
		{name: "CJK no newline", markdown: "# 并发\n通道传递消息", headings: 1},
		{name: "existing YAML", markdown: "---\ntags: [go, db]\nalias: demo\n---\n# YAML\nbody\n", headings: 1},
	}
	canonicalizer := NewCanonicalizer()
	for _, fixture := range corpus {
		t.Run(fixture.name, func(t *testing.T) {
			inspected, err := canonicalizer.Inspect(fixture.markdown)
			if err != nil {
				t.Fatal(err)
			}
			if len(inspected.DraftNodes) != fixture.headings {
				t.Fatalf("heading count = %d, want %d", len(inspected.DraftNodes), fixture.headings)
			}
			nodeIDs := make([]string, fixture.headings)
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
			for _, expectedText := range semanticFragments(fixture.markdown) {
				if !strings.Contains(document.CanonicalMarkdown, expectedText) {
					t.Fatalf("canonical Markdown lost %q: %q", expectedText, document.CanonicalMarkdown)
				}
			}
			for _, node := range document.Nodes {
				for _, sourceRange := range []SourceRange{node.HeadingRange, node.LocalBodyRange, node.SectionRange} {
					_ = document.CanonicalMarkdown[sourceRange.Start:sourceRange.End]
				}
			}
		})
	}
}

func semanticFragments(markdown string) []string {
	var result []string
	for _, line := range strings.Split(strings.ReplaceAll(markdown, "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "---" {
			continue
		}
		result = append(result, line)
	}
	return result
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
