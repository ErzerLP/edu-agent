package agentui

// Copy/move/mkdir paths and metadata are an authorization contract. The bounded
// selector excerpt must not authorize a silently shortened mutation plan. Page the
// complete frozen body and gate approval until its final page was rendered.
func (s *selectorModel) copyPreviewPage(width, rows int) []string {
	if width != s.copyWidth || rows != s.copyRows {
		s.copyPage = 0
		s.copyWidth = width
		s.copyRows = rows
		s.copyPageRendered = false
	}
	// Preparation already bounds the entire preview. This bound exceeds its
	// maximum possible wrapped rows even at the terminal's minimum width.
	lines := wrapDisplayLines(s.body, width, 16<<10)
	s.copyPages = max(1, (len(lines)+rows-1)/rows)
	s.copyPage = min(s.copyPage, s.copyPages-1)
	start := s.copyPage * rows
	s.copyPageRendered = true
	return lines[start:min(len(lines), start+rows)]
}
