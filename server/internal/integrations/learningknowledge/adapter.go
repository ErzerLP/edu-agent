package learningknowledge

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
)

type TreeReader interface {
	Tree(context.Context, string) (knowledge.TreeResult, error)
}
type Adapter struct{ reader TreeReader }

func New(reader TreeReader) *Adapter { return &Adapter{reader: reader} }
func (a *Adapter) Resolve(ctx context.Context, knowledgeRevisionID, nodeRevisionID string) (learning.KnowledgeReference, error) {
	tree, err := a.reader.Tree(ctx, knowledgeRevisionID)
	if err != nil {
		return learning.KnowledgeReference{}, err
	}
	if tree.Revision.ID != knowledgeRevisionID {
		return learning.KnowledgeReference{}, invalidReference("knowledge revision mismatch")
	}
	for _, document := range tree.Revision.Documents {
		for _, node := range document.Revision.Nodes {
			if node.ID != nodeRevisionID {
				continue
			}
			if node.NodeID == "" || document.Revision.ID == "" || node.DocumentRevisionID != document.Revision.ID {
				return learning.KnowledgeReference{}, invalidReference("node revision ownership mismatch")
			}
			markdown := document.Revision.CanonicalMarkdown
			start, end := node.SectionRange.Start, node.SectionRange.End
			if !utf8.ValidString(markdown) || start < 0 || end <= start || end > len(markdown) || !utf8Boundary(markdown, start) || !utf8Boundary(markdown, end) {
				return learning.KnowledgeReference{}, invalidReference("invalid stored node source range")
			}
			slice := markdown[start:end]
			if !utf8.ValidString(slice) {
				return learning.KnowledgeReference{}, invalidReference("invalid stored node source slice")
			}
			return learning.KnowledgeReference{KnowledgeRevisionID: tree.Revision.ID, NodeID: node.NodeID, NodeRevisionID: node.ID, DocumentRevisionID: document.Revision.ID, Range: learning.SourceRange{Start: start, End: end}, Slice: slice, SliceSHA256: learning.SHA256([]byte(slice))}, nil
		}
	}
	return learning.KnowledgeReference{}, invalidReference("node revision not found")
}

func utf8Boundary(value string, offset int) bool {
	return offset == 0 || offset == len(value) || utf8.RuneStart(value[offset])
}

func invalidReference(reason string) error {
	return &learning.Error{Code: learning.CodeKnowledgeReferenceInvalid, Cause: fmt.Errorf("%s", reason)}
}
