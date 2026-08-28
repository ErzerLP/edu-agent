package learningknowledge

import (
	"context"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
)

type fakeTreeReader struct {
	result knowledge.TreeResult
	err    error
}

func (f fakeTreeReader) Tree(context.Context, string) (knowledge.TreeResult, error) {
	return f.result, f.err
}

func validTree(markdown string, sourceRange knowledge.SourceRange) knowledge.TreeResult {
	return knowledge.TreeResult{Revision: knowledge.KnowledgeRevision{
		ID: "knowledge-revision",
		Documents: []knowledge.SnapshotDocument{{Revision: knowledge.DocumentRevision{
			ID:                "document-revision",
			CanonicalMarkdown: markdown,
			Nodes: []knowledge.NodeRevision{{
				ID:                 "node-revision",
				NodeID:             "node",
				DocumentRevisionID: "document-revision",
				SectionRange:       sourceRange,
			}},
		}}},
	}}
}

func TestResolveReturnsCanonicalReference(t *testing.T) {
	result := validTree("prefix é suffix", knowledge.SourceRange{Start: 7, End: 9})
	reference, err := New(fakeTreeReader{result: result}).Resolve(context.Background(), "knowledge-revision", "node-revision")
	if err != nil {
		t.Fatal(err)
	}
	if reference.KnowledgeRevisionID != "knowledge-revision" || reference.NodeID != "node" || reference.NodeRevisionID != "node-revision" || reference.DocumentRevisionID != "document-revision" || reference.Slice != "é" || reference.SliceSHA256 != learning.SHA256([]byte("é")) {
		t.Fatalf("reference = %+v", reference)
	}
}

func TestResolveRejectsWrongReturnedRevisionAndOwnership(t *testing.T) {
	cases := map[string]func(*knowledge.TreeResult){
		"wrong revision": func(tree *knowledge.TreeResult) {
			tree.Revision.ID = "other-revision"
		},
		"missing node owner": func(tree *knowledge.TreeResult) {
			tree.Revision.Documents[0].Revision.Nodes[0].NodeID = ""
		},
		"wrong document owner": func(tree *knowledge.TreeResult) {
			tree.Revision.Documents[0].Revision.Nodes[0].DocumentRevisionID = "other-document"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			tree := validTree("text", knowledge.SourceRange{Start: 0, End: 4})
			mutate(&tree)
			_, err := New(fakeTreeReader{result: tree}).Resolve(context.Background(), "knowledge-revision", "node-revision")
			if learning.ErrorCode(err) != learning.CodeKnowledgeReferenceInvalid {
				t.Fatalf("error = %v, code = %q", err, learning.ErrorCode(err))
			}
		})
	}
}

func TestResolveRejectsInvalidUTF8AndMultibyteTruncation(t *testing.T) {
	cases := []struct {
		name     string
		markdown string
		start    int
		end      int
	}{
		{name: "start inside rune", markdown: "aéb", start: 2, end: 4},
		{name: "end inside rune", markdown: "aéb", start: 0, end: 2},
		{name: "invalid stored utf8", markdown: string([]byte{'a', 0xff, 'b'}), start: 0, end: 3},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			tree := validTree(test.markdown, knowledge.SourceRange{Start: test.start, End: test.end})
			_, err := New(fakeTreeReader{result: tree}).Resolve(context.Background(), "knowledge-revision", "node-revision")
			if learning.ErrorCode(err) != learning.CodeKnowledgeReferenceInvalid {
				t.Fatalf("error = %v, code = %q", err, learning.ErrorCode(err))
			}
		})
	}
}
