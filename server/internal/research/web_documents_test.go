package research

import (
	"testing"
	"time"
)

// A search snippet, a forged range, or an unread fragment must never become evidence.
func TestWebDocumentEvidenceBoundary(t *testing.T) {
	now := time.Now()
	source := Source{ID: "resource", RevisionID: "revision", Locator: "https://example.org/a", FinalURL: "https://example.org/a", Text: "真实正文。第二句。", StorageAllowed: true, Status: "parsed", FetchedAt: &now, Coverage: "complete_text", Fragments: []Fragment{{ID: "one", Start: 0, End: 15, Text: "真实正文。"}, {ID: "two", Start: 15, End: 27, Text: "第二句。"}}}
	document, err := NewWebDocument("document", source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = document.Cite("one", "真实正文", []string{"one"}); err != nil {
		t.Fatal(err)
	}
	if _, err = document.Cite("two", "第二句", []string{"one"}); err == nil {
		t.Fatal("unread fragment accepted")
	}
	if _, err = document.Cite("one", "搜索摘要", []string{"one"}); err == nil {
		t.Fatal("invented quote accepted")
	}
	source.StorageAllowed = false
	if _, err = NewWebDocument("document", source); err == nil {
		t.Fatal("restricted content accepted")
	}
	source.StorageAllowed = true
	source.Fragments[0].End = 14
	if _, err = NewWebDocument("document", source); err == nil {
		t.Fatal("invalid UTF-8 range accepted")
	}
}
