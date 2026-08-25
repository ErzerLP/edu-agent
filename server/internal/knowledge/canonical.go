package knowledge

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
	"go.yaml.in/yaml/v3"
)

var (
	documentRevisionNamespace = uuid.MustParse("d750d9c6-6ff8-5f43-8ebf-820335c8c4bd")
	nodeRevisionNamespace     = uuid.MustParse("09cfa07d-0b25-5c53-a4d6-d4e31c64c79c")
	markerPattern             = regexp.MustCompile(`^<!-- edu-agent-node:v1 (\{.*\}) -->$`)
	setextPattern             = regexp.MustCompile(`^[ \t]{0,3}(=+|-+)[ \t]*(?:\n|$)`)
)

type DraftNode struct {
	Preorder              int
	Level                 int
	Title                 string
	AncestorTitles        []string
	ExplicitNodeID        string
	SemanticLocalBodyHash string
	VisibleTokens         []string
}

type InspectedDocument struct {
	Normalized          string
	Body                string
	UserFrontMatter     string
	UserFrontMatterFlow bool
	ExplicitDocumentID  string
	ExplicitRootNodeID  string
	SemanticHash        string
	DraftNodes          []DraftNode
}

type Canonicalizer struct {
	markdown goldmark.Markdown
}

func NewCanonicalizer() *Canonicalizer {
	return &Canonicalizer{markdown: goldmark.New(goldmark.WithExtensions(extension.GFM))}
}

func (c *Canonicalizer) Inspect(markdown string) (InspectedDocument, error) {
	normalized, err := normalizeMarkdown(markdown)
	if err != nil {
		return InspectedDocument{}, err
	}
	front, body, err := parseFrontMatter(normalized)
	if err != nil {
		return InspectedDocument{}, err
	}
	cleanBody, explicitMarkers, err := c.stripIdentityMarkers([]byte(body))
	if err != nil {
		return InspectedDocument{}, err
	}
	headings := c.headingBlocks(cleanBody)
	drafts := make([]DraftNode, len(headings))
	for i, heading := range headings {
		drafts[i] = DraftNode{Preorder: i, Level: heading.level, Title: heading.title, ExplicitNodeID: explicitMarkers[i]}
	}
	populateDraftBodies(drafts, headings, cleanBody)
	semantic := cleanBody
	if front.user != "" {
		semantic = []byte("---\n" + front.user + "---\n" + string(cleanBody))
	}
	return InspectedDocument{
		Normalized: string(normalized), Body: string(cleanBody), UserFrontMatter: front.user,
		UserFrontMatterFlow: front.flow, ExplicitDocumentID: front.documentID, ExplicitRootNodeID: front.rootNodeID,
		SemanticHash: sha256Hex(semantic), DraftNodes: drafts,
	}, nil
}

func (c *Canonicalizer) Materialize(inspected InspectedDocument, documentID, rootNodeID string, nodeIDs []string) (DocumentRevision, error) {
	if !validUUID(documentID) || !validUUID(rootNodeID) || len(nodeIDs) != len(inspected.DraftNodes) {
		return DocumentRevision{}, &Error{Code: CodeInvalidRequest}
	}
	seen := map[string]struct{}{rootNodeID: {}}
	for _, id := range nodeIDs {
		if !validUUID(id) {
			return DocumentRevision{}, &Error{Code: CodeInvalidIdentityMarker}
		}
		if _, exists := seen[id]; exists {
			return DocumentRevision{}, &Error{Code: CodeInvalidIdentityMarker}
		}
		seen[id] = struct{}{}
	}
	body := []byte(inspected.Body)
	headings := c.headingBlocks(body)
	if len(headings) != len(nodeIDs) {
		return DocumentRevision{}, &Error{Code: CodeInvalidMarkdown}
	}
	for i := len(headings) - 1; i >= 0; i-- {
		marker := []byte(fmt.Sprintf("<!-- edu-agent-node:v1 {\"id\":\"%s\"} -->\n", nodeIDs[i]))
		position := headings[i].start
		body = append(body[:position], append(marker, body[position:]...)...)
	}
	var envelope strings.Builder
	if err := writeIdentityFrontMatter(&envelope, documentID, rootNodeID, "", inspected.UserFrontMatter, inspected.UserFrontMatterFlow); err != nil {
		return DocumentRevision{}, err
	}
	canonical := envelope.String() + string(body)
	return c.Index(canonical)
}

func (c *Canonicalizer) Index(canonical string) (DocumentRevision, error) {
	if !utf8.ValidString(canonical) {
		return DocumentRevision{}, &Error{Code: CodeInvalidMarkdown}
	}
	front, body, err := parseFrontMatter([]byte(canonical))
	if err != nil || !validUUID(front.documentID) || !validUUID(front.rootNodeID) {
		return DocumentRevision{}, &Error{Code: CodeInvalidMarkdown, Cause: err}
	}
	prefixLength := len(canonical) - len(body)
	bodyBytes := []byte(body)
	markerStarts, nodeIDs, headings, err := c.canonicalHeadingMarkers(bodyBytes)
	if err != nil {
		return DocumentRevision{}, err
	}
	canonicalHash := sha256Hex([]byte(canonical))
	documentRevisionID := uuid.NewSHA1(documentRevisionNamespace, []byte(front.documentID+"\n"+canonicalHash+"\n"+ParserVersion)).String()
	nodes := make([]NodeRevision, len(headings)+1)
	rootID := uuid.NewSHA1(nodeRevisionNamespace, []byte(documentRevisionID+"\n"+front.rootNodeID+"\n"+IndexerVersion)).String()
	firstSection := len(canonical)
	if len(markerStarts) > 0 {
		firstSection = prefixLength + markerStarts[0]
	}
	nodes[0] = NodeRevision{
		ID: rootID, NodeID: front.rootNodeID, DocumentRevisionID: documentRevisionID,
		SiblingIndex: 0, HeadingLevel: 0, AncestorTitles: []string{},
		HeadingRange:          rangeFor([]byte(canonical), prefixLength, prefixLength),
		LocalBodyRange:        rangeFor([]byte(canonical), prefixLength, firstSection),
		SectionRange:          rangeFor([]byte(canonical), prefixLength, len(canonical)),
		SemanticLocalBodyHash: sha256Hex(visibleMarkdown([]byte(canonical[prefixLength:firstSection]), c.markdown)),
	}
	type stackEntry struct{ level, index int }
	stack := []stackEntry{{level: 0, index: 0}}
	siblingCounts := map[int]int{}
	for i, heading := range headings {
		for len(stack) > 1 && stack[len(stack)-1].level >= heading.level {
			stack = stack[:len(stack)-1]
		}
		parentIndex := stack[len(stack)-1].index
		sibling := siblingCounts[parentIndex]
		siblingCounts[parentIndex]++
		sectionStart := prefixLength + markerStarts[i]
		headingStart := prefixLength + heading.start
		headingEnd := prefixLength + heading.end
		localEnd := len(canonical)
		if i+1 < len(headings) {
			localEnd = prefixLength + markerStarts[i+1]
		}
		sectionEnd := len(canonical)
		for j := i + 1; j < len(headings); j++ {
			if headings[j].level <= heading.level {
				sectionEnd = prefixLength + markerStarts[j]
				break
			}
		}
		parentRevisionID := nodes[parentIndex].ID
		ancestorTitles := append([]string{}, nodes[parentIndex].AncestorTitles...)
		if nodes[parentIndex].HeadingLevel != 0 {
			ancestorTitles = append(ancestorTitles, nodes[parentIndex].Title)
		}
		nodeRevisionID := uuid.NewSHA1(nodeRevisionNamespace, []byte(documentRevisionID+"\n"+nodeIDs[i]+"\n"+IndexerVersion)).String()
		nodes[i+1] = NodeRevision{
			ID: nodeRevisionID, NodeID: nodeIDs[i], DocumentRevisionID: documentRevisionID,
			ParentNodeRevisionID: &parentRevisionID, SiblingIndex: sibling, HeadingLevel: heading.level,
			Title: heading.title, AncestorTitles: ancestorTitles,
			HeadingRange:          rangeFor([]byte(canonical), headingStart, headingEnd),
			LocalBodyRange:        rangeFor([]byte(canonical), headingEnd, localEnd),
			SectionRange:          rangeFor([]byte(canonical), sectionStart, sectionEnd),
			SemanticLocalBodyHash: sha256Hex(visibleMarkdown([]byte(canonical[headingEnd:localEnd]), c.markdown)),
		}
		nodes[parentIndex].Children = append(nodes[parentIndex].Children, nodeRevisionID)
		stack = append(stack, stackEntry{level: heading.level, index: i + 1})
	}
	return DocumentRevision{
		ID: documentRevisionID, DocumentID: front.documentID, RootNodeID: front.rootNodeID,
		CanonicalHash: canonicalHash, SemanticHash: semanticHashForCanonical(canonical, c),
		CanonicalMarkdown: canonical, Nodes: nodes,
	}, nil
}

func ExportMarkdown(canonical, revisionID string) (string, error) {
	if !validUUID(revisionID) {
		return "", &Error{Code: CodeInvalidRequest}
	}
	front, body, err := parseFrontMatter([]byte(canonical))
	if err != nil || front.documentID == "" {
		return "", &Error{Code: CodeInvalidMarkdown, Cause: err}
	}
	var envelope strings.Builder
	if err := writeIdentityFrontMatter(&envelope, front.documentID, front.rootNodeID, revisionID, front.user, front.flow); err != nil {
		return "", err
	}
	envelope.WriteString(body)
	return envelope.String(), nil
}

func writeIdentityFrontMatter(builder *strings.Builder, documentID, rootNodeID, sourceRevisionID, user string, flow bool) error {
	builder.WriteString("---\n")
	if flow {
		inner := user
		builder.WriteString("{edu-agent-format: 1, edu-agent-document-id: ")
		builder.WriteString(documentID)
		builder.WriteString(", edu-agent-root-node-id: ")
		builder.WriteString(rootNodeID)
		if sourceRevisionID != "" {
			builder.WriteString(", edu-agent-source-revision-id: ")
			builder.WriteString(sourceRevisionID)
		}
		if strings.TrimSpace(inner) != "" {
			builder.WriteByte(',')
			builder.WriteString(inner)
		}
		builder.WriteString("}\n---\n")
		return nil
	}
	builder.WriteString("edu-agent-format: 1\n")
	builder.WriteString("edu-agent-document-id: ")
	builder.WriteString(documentID)
	builder.WriteByte('\n')
	builder.WriteString("edu-agent-root-node-id: ")
	builder.WriteString(rootNodeID)
	builder.WriteByte('\n')
	if sourceRevisionID != "" {
		builder.WriteString("edu-agent-source-revision-id: ")
		builder.WriteString(sourceRevisionID)
		builder.WriteByte('\n')
	}
	if user != "" {
		builder.WriteString(user)
		if !strings.HasSuffix(user, "\n") {
			builder.WriteByte('\n')
		}
	}
	builder.WriteString("---\n")
	return nil
}

type frontMatter struct {
	user       string
	flow       bool
	documentID string
	rootNodeID string
}

func normalizeMarkdown(markdown string) ([]byte, error) {
	value := []byte(markdown)
	if !utf8.Valid(value) || bytes.IndexByte(value, 0) >= 0 {
		return nil, &Error{Code: CodeInvalidMarkdown}
	}
	if len(value) > MaxDocumentBytes {
		return nil, &Error{Code: CodePayloadTooLarge}
	}
	if bytes.HasPrefix(value, []byte{0xef, 0xbb, 0xbf}) {
		value = value[3:]
	}
	value = bytes.ReplaceAll(value, []byte("\r\n"), []byte("\n"))
	value = bytes.ReplaceAll(value, []byte("\r"), []byte("\n"))
	return value, nil
}

func parseFrontMatter(markdown []byte) (frontMatter, string, error) {
	if !bytes.HasPrefix(markdown, []byte("---\n")) {
		return frontMatter{}, string(markdown), nil
	}
	closingStart, closingEnd := -1, -1
	for offset := 4; offset <= len(markdown); {
		lineEnd := bytes.IndexByte(markdown[offset:], '\n')
		end := len(markdown)
		if lineEnd >= 0 {
			end = offset + lineEnd + 1
		}
		trimmed := strings.TrimSpace(string(markdown[offset:end]))
		if trimmed == "---" || trimmed == "..." {
			closingStart, closingEnd = offset, end
			break
		}
		if end == len(markdown) {
			break
		}
		offset = end
	}
	if closingStart < 0 {
		return frontMatter{}, "", &Error{Code: CodeInvalidMarkdown, Cause: fmt.Errorf("unterminated YAML frontmatter")}
	}
	yamlBytes := markdown[4:closingStart]
	var root yaml.Node
	if err := yaml.Unmarshal(yamlBytes, &root); err != nil {
		return frontMatter{}, "", &Error{Code: CodeInvalidMarkdown, Cause: fmt.Errorf("parse YAML frontmatter: %w", err)}
	}
	result := frontMatter{}
	removeLines := map[int]bool{}
	if len(root.Content) > 0 {
		mapping := root.Content[0]
		if mapping.Kind != yaml.MappingNode {
			return frontMatter{}, "", &Error{Code: CodeInvalidMarkdown, Cause: fmt.Errorf("frontmatter must be a mapping")}
		}
		flow := mapping.Style&yaml.FlowStyle != 0
		var flowPairs []string
		if flow {
			var splitErr error
			flowPairs, splitErr = splitFlowMappingPairs(yamlBytes)
			if splitErr != nil || len(flowPairs) != len(mapping.Content)/2 {
				return frontMatter{}, "", &Error{Code: CodeInvalidMarkdown, Cause: fmt.Errorf("preserve flow-style frontmatter pairs: %w", splitErr)}
			}
		}
		seen := map[string]struct{}{}
		for i := 0; i+1 < len(mapping.Content); i += 2 {
			key, value := mapping.Content[i], mapping.Content[i+1]
			if key.Kind != yaml.ScalarNode {
				return frontMatter{}, "", &Error{Code: CodeInvalidMarkdown, Cause: fmt.Errorf("frontmatter key must be scalar")}
			}
			if _, exists := seen[key.Value]; exists {
				return frontMatter{}, "", &Error{Code: CodeInvalidMarkdown, Cause: fmt.Errorf("duplicate frontmatter key")}
			}
			seen[key.Value] = struct{}{}
			reserved := isReservedFrontMatterKey(key.Value)
			if reserved {
				if value.Kind != yaml.ScalarNode || value.Style == yaml.LiteralStyle || value.Style == yaml.FoldedStyle || key.Line != value.Line {
					return frontMatter{}, "", &Error{Code: CodeInvalidMarkdown, Cause: fmt.Errorf("reserved frontmatter value must be a single-line scalar")}
				}
				if !flow {
					removeLines[key.Line] = true
				}
			}
			switch key.Value {
			case "edu-agent-format":
				format, err := strconv.Atoi(value.Value)
				if err != nil || format != 1 {
					return frontMatter{}, "", &Error{Code: CodeInvalidMarkdown, Cause: fmt.Errorf("unsupported identity envelope")}
				}
			case "edu-agent-document-id":
				if !validUUID(value.Value) {
					return frontMatter{}, "", &Error{Code: CodeInvalidMarkdown, Cause: fmt.Errorf("invalid document identity")}
				}
				result.documentID = strings.ToLower(value.Value)
			case "edu-agent-root-node-id":
				if !validUUID(value.Value) {
					return frontMatter{}, "", &Error{Code: CodeInvalidMarkdown, Cause: fmt.Errorf("invalid root node identity")}
				}
				result.rootNodeID = strings.ToLower(value.Value)
			case "edu-agent-source-revision-id":
				if !validUUID(value.Value) {
					return frontMatter{}, "", &Error{Code: CodeInvalidMarkdown, Cause: fmt.Errorf("invalid source revision identity")}
				}
			}
		}
		if flow {
			result.flow = true
			var userPairs []string
			for i := 0; i+1 < len(mapping.Content); i += 2 {
				if !isReservedFrontMatterKey(mapping.Content[i].Value) {
					userPairs = append(userPairs, flowPairs[i/2])
				}
			}
			result.user = strings.Join(userPairs, ",")
		}
	}
	if !result.flow {
		lines := strings.SplitAfter(string(yamlBytes), "\n")
		var user strings.Builder
		for i, line := range lines {
			if line == "" || removeLines[i+1] {
				continue
			}
			user.WriteString(line)
		}
		result.user = user.String()
	}
	return result, string(markdown[closingEnd:]), nil
}

func splitFlowMappingPairs(source []byte) ([]string, error) {
	start := 0
	for start < len(source) && (source[start] == ' ' || source[start] == '\t' || source[start] == '\n' || source[start] == '\r') {
		start++
	}
	end := len(source)
	for end > start && (source[end-1] == ' ' || source[end-1] == '\t' || source[end-1] == '\n' || source[end-1] == '\r') {
		end--
	}
	if end-start < 2 || source[start] != '{' || source[end-1] != '}' {
		return nil, fmt.Errorf("flow mapping is not enclosed")
	}
	inner := source[start+1 : end-1]
	if strings.TrimSpace(string(inner)) == "" {
		return []string{}, nil
	}
	pairs := make([]string, 0, 4)
	segmentStart := 0
	depth := 0
	var quote byte
	inComment := false
	for i := 0; i < len(inner); i++ {
		ch := inner[i]
		if inComment {
			if ch == '\n' || ch == '\r' {
				inComment = false
			}
			continue
		}
		if quote == '\'' {
			if ch == '\'' {
				if i+1 < len(inner) && inner[i+1] == '\'' {
					i++
				} else {
					quote = 0
				}
			}
			continue
		}
		if quote == '"' {
			if ch == '\\' {
				i++
			} else if ch == '"' {
				quote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
		case '#':
			if i == 0 || inner[i-1] == ' ' || inner[i-1] == '\t' {
				inComment = true
			}
		case '[', '{':
			depth++
		case ']', '}':
			if depth == 0 {
				return nil, fmt.Errorf("unbalanced flow mapping")
			}
			depth--
		case ',':
			if depth == 0 {
				if strings.TrimSpace(string(inner[segmentStart:i])) == "" {
					return nil, fmt.Errorf("empty flow mapping pair")
				}
				pairs = append(pairs, string(inner[segmentStart:i]))
				segmentStart = i + 1
			}
		}
	}
	if quote != 0 || depth != 0 {
		return nil, fmt.Errorf("unterminated flow value")
	}
	last := string(inner[segmentStart:])
	if strings.TrimSpace(last) != "" {
		pairs = append(pairs, last)
	}
	return pairs, nil
}

func isReservedFrontMatterKey(value string) bool {
	switch value {
	case "edu-agent-format", "edu-agent-document-id", "edu-agent-root-node-id", "edu-agent-source-revision-id":
		return true
	default:
		return false
	}
}

type headingBlock struct {
	start int
	end   int
	level int
	title string
}

func (c *Canonicalizer) headingBlocks(source []byte) []headingBlock {
	document := c.markdown.Parser().Parse(text.NewReader(source))
	var result []headingBlock
	for node := document.FirstChild(); node != nil; node = node.NextSibling() {
		heading, ok := node.(*ast.Heading)
		if !ok || heading.Lines().Len() == 0 {
			continue
		}
		start := lineStart(source, heading.Lines().At(0).Start)
		end := lineEnd(source, heading.Lines().At(heading.Lines().Len()-1).Stop)
		line := source[start:end]
		trimmed := bytes.TrimLeft(line, " \t")
		if len(trimmed) == 0 || trimmed[0] != '#' {
			nextEnd := lineEnd(source, end)
			if nextEnd > end && setextPattern.Match(source[end:nextEnd]) {
				end = nextEnd
			}
		}
		result = append(result, headingBlock{start: start, end: end, level: heading.Level, title: strings.TrimSpace(string(heading.Text(source)))})
	}
	return result
}

func (c *Canonicalizer) stripIdentityMarkers(source []byte) ([]byte, map[int]string, error) {
	document := c.markdown.Parser().Parse(text.NewReader(source))
	type removal struct{ start, end int }
	var removals []removal
	explicit := map[int]string{}
	seen := map[string]struct{}{}
	headingOrdinal := 0
	for node := document.FirstChild(); node != nil; node = node.NextSibling() {
		if heading, ok := node.(*ast.Heading); ok {
			_ = heading
			headingOrdinal++
			continue
		}
		html, ok := node.(*ast.HTMLBlock)
		if !ok {
			continue
		}
		raw := strings.TrimSpace(string(html.Text(source)))
		if !strings.Contains(raw, "edu-agent-node:") {
			continue
		}
		match := markerPattern.FindStringSubmatch(raw)
		if len(match) != 2 {
			return nil, nil, &Error{Code: CodeInvalidIdentityMarker}
		}
		markerID, err := parseMarkerPayload(match[1])
		if err != nil {
			return nil, nil, &Error{Code: CodeInvalidIdentityMarker, Cause: err}
		}
		next := node.NextSibling()
		if _, ok := next.(*ast.Heading); !ok {
			return nil, nil, &Error{Code: CodeInvalidIdentityMarker}
		}
		if _, duplicate := explicit[headingOrdinal]; duplicate {
			return nil, nil, &Error{Code: CodeInvalidIdentityMarker}
		}
		markerID = strings.ToLower(markerID)
		if _, duplicate := seen[markerID]; duplicate {
			return nil, nil, &Error{Code: CodeInvalidIdentityMarker}
		}
		seen[markerID] = struct{}{}
		explicit[headingOrdinal] = markerID
		start := lineStart(source, html.Lines().At(0).Start)
		end := physicalLineEnd(source, html.Lines().At(html.Lines().Len()-1).Stop)
		if html.HasClosure() {
			end = physicalLineEnd(source, html.ClosureLine.Stop)
		}
		removals = append(removals, removal{start: start, end: end})
	}
	sort.Slice(removals, func(i, j int) bool { return removals[i].start > removals[j].start })
	clean := append([]byte(nil), source...)
	for _, item := range removals {
		clean = append(clean[:item.start], clean[item.end:]...)
	}
	return clean, explicit, nil
}

func parseMarkerPayload(payload string) (string, error) {
	decoder := json.NewDecoder(strings.NewReader(payload))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return "", fmt.Errorf("marker payload must be an object")
	}
	seen := map[string]struct{}{}
	var id string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return "", fmt.Errorf("read marker key: %w", err)
		}
		key, ok := token.(string)
		if !ok {
			return "", fmt.Errorf("marker key must be a string")
		}
		if _, duplicate := seen[key]; duplicate {
			return "", fmt.Errorf("duplicate marker key")
		}
		seen[key] = struct{}{}
		if key != "id" {
			return "", fmt.Errorf("unknown marker key")
		}
		if err := decoder.Decode(&id); err != nil {
			return "", fmt.Errorf("marker id must be a string: %w", err)
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return "", fmt.Errorf("marker payload is not closed")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return "", fmt.Errorf("marker payload contains trailing JSON")
	}
	if !validUUID(id) {
		return "", fmt.Errorf("marker id is not a UUID")
	}
	return strings.ToLower(id), nil
}

func (c *Canonicalizer) canonicalHeadingMarkers(source []byte) ([]int, []string, []headingBlock, error) {
	clean, explicit, err := c.stripIdentityMarkers(source)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(clean) == len(source) && len(c.headingBlocks(source)) != 0 {
		return nil, nil, nil, &Error{Code: CodeInvalidIdentityMarker, Cause: fmt.Errorf("canonical heading is missing its identity marker")}
	}
	cleanHeadings := c.headingBlocks(clean)
	if len(explicit) != len(cleanHeadings) {
		return nil, nil, nil, &Error{Code: CodeInvalidIdentityMarker, Cause: fmt.Errorf("canonical marker count does not match heading count")}
	}
	// Re-read canonical source to retain offsets after marker insertion.
	document := c.markdown.Parser().Parse(text.NewReader(source))
	var starts, ids []string
	_ = starts
	markerStarts := make([]int, 0, len(cleanHeadings))
	ids = make([]string, 0, len(cleanHeadings))
	var pendingStart = -1
	var pendingID string
	var headings []headingBlock
	for node := document.FirstChild(); node != nil; node = node.NextSibling() {
		if html, ok := node.(*ast.HTMLBlock); ok {
			raw := strings.TrimSpace(string(html.Text(source)))
			match := markerPattern.FindStringSubmatch(raw)
			if len(match) == 2 {
				markerID, err := parseMarkerPayload(match[1])
				if err != nil {
					return nil, nil, nil, &Error{Code: CodeInvalidIdentityMarker, Cause: err}
				}
				pendingStart = lineStart(source, html.Lines().At(0).Start)
				pendingID = markerID
			}
			continue
		}
		heading, ok := node.(*ast.Heading)
		if !ok {
			continue
		}
		if pendingStart < 0 || pendingID == "" {
			return nil, nil, nil, &Error{Code: CodeInvalidIdentityMarker, Cause: fmt.Errorf("canonical heading marker is not its preceding AST sibling")}
		}
		start := lineStart(source, heading.Lines().At(0).Start)
		end := lineEnd(source, heading.Lines().At(heading.Lines().Len()-1).Stop)
		trimmed := bytes.TrimLeft(source[start:end], " \t")
		if len(trimmed) == 0 || trimmed[0] != '#' {
			nextEnd := lineEnd(source, end)
			if nextEnd > end && setextPattern.Match(source[end:nextEnd]) {
				end = nextEnd
			}
		}
		markerStarts = append(markerStarts, pendingStart)
		ids = append(ids, pendingID)
		headings = append(headings, headingBlock{start: start, end: end, level: heading.Level, title: strings.TrimSpace(string(heading.Text(source)))})
		pendingStart, pendingID = -1, ""
	}
	return markerStarts, ids, headings, nil
}

func populateDraftBodies(drafts []DraftNode, headings []headingBlock, source []byte) {
	stack := []int{}
	for i := range drafts {
		for len(stack) > 0 && drafts[stack[len(stack)-1]].Level >= drafts[i].Level {
			stack = stack[:len(stack)-1]
		}
		for _, ancestor := range stack {
			drafts[i].AncestorTitles = append(drafts[i].AncestorTitles, drafts[ancestor].Title)
		}
		stack = append(stack, i)
		end := len(source)
		if i+1 < len(headings) {
			end = headings[i+1].start
		}
		visible := visibleMarkdown(source[headings[i].end:end], goldmark.New(goldmark.WithExtensions(extension.GFM)))
		drafts[i].SemanticLocalBodyHash = sha256Hex(visible)
		drafts[i].VisibleTokens = identityTokens(string(visible))
	}
}

func semanticHashForCanonical(canonical string, c *Canonicalizer) string {
	front, body, err := parseFrontMatter([]byte(canonical))
	if err != nil {
		return ""
	}
	clean, _, err := c.stripIdentityMarkers([]byte(body))
	if err != nil {
		return ""
	}
	semantic := clean
	if front.user != "" {
		semantic = []byte("---\n" + front.user + "---\n" + string(clean))
	}
	return sha256Hex(semantic)
}

func visibleMarkdown(source []byte, markdown goldmark.Markdown) []byte {
	document := markdown.Parser().Parse(text.NewReader(source))
	var result bytes.Buffer
	_ = ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch typed := node.(type) {
		case *ast.Text:
			result.Write(typed.Segment.Value(source))
			result.WriteByte(' ')
		case *ast.String:
			result.Write(typed.Value)
			result.WriteByte(' ')
		case *ast.CodeBlock:
			result.Write(typed.Lines().Value(source))
			result.WriteByte(' ')
		case *ast.FencedCodeBlock:
			result.Write(typed.Lines().Value(source))
			result.WriteByte(' ')
		}
		return ast.WalkContinue, nil
	})
	return bytes.TrimSpace(result.Bytes())
}

func rangeFor(source []byte, start, end int) SourceRange {
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	if end > len(source) {
		end = len(source)
	}
	startLine := 1 + bytes.Count(source[:start], []byte("\n"))
	endOffset := end
	if end > start && source[end-1] == '\n' {
		endOffset--
	}
	endLine := 1 + bytes.Count(source[:endOffset], []byte("\n"))
	return SourceRange{Start: start, End: end, StartLine: startLine, EndLine: endLine}
}

func lineStart(source []byte, position int) int {
	if position > len(source) {
		position = len(source)
	}
	if index := bytes.LastIndexByte(source[:position], '\n'); index >= 0 {
		return index + 1
	}
	return 0
}

func physicalLineEnd(source []byte, position int) int {
	if position > 0 && position <= len(source) && source[position-1] == '\n' {
		return position
	}
	return lineEnd(source, position)
}

func lineEnd(source []byte, position int) int {
	if position < 0 {
		position = 0
	}
	if position > len(source) {
		position = len(source)
	}
	if index := bytes.IndexByte(source[position:], '\n'); index >= 0 {
		return position + index + 1
	}
	return len(source)
}

func validUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == strings.ToLower(value)
}

func sha256Hex(value []byte) string {
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:])
}
