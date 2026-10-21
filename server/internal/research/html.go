package research

import (
	"bytes"
	"io"
	"mime"
	"net/url"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// Decode before extraction, but fingerprint the original response bytes.
// A declared charset must be supported; UTF-8 is validated rather than repaired.
func decodeText(body []byte, media, label string) ([]byte, string) {
	encoding, name := charset.Lookup(label)
	if label != "" && encoding == nil {
		return nil, "unsupported_encoding"
	}
	if label == "" && media == "text/html" {
		var certain bool
		encoding, name, certain = charset.DetermineEncoding(body, "")
		if !certain && utf8.Valid(body) && !hasHTMLCharset(body) {
			name = "utf-8"
		}
	} else if label == "" {
		name = "utf-8"
	}
	if name != "utf-8" && name != "us-ascii" {
		var err error
		body, err = io.ReadAll(io.LimitReader(encoding.NewDecoder().Reader(bytes.NewReader(body)), MaxDecoded+1))
		if err != nil {
			return nil, "unsupported_encoding"
		}
		if len(body) > MaxDecoded {
			return nil, "decoded_limit"
		}
	}
	if !utf8.Valid(body) || bytes.ContainsRune(body, 0) {
		return nil, "unsupported_encoding"
	}
	return bytes.TrimPrefix(body, []byte("\xef\xbb\xbf")), ""
}

func hasHTMLCharset(body []byte) bool {
	z := html.NewTokenizer(bytes.NewReader(body[:min(len(body), 1024)]))
	for {
		kind := z.Next()
		if kind == html.ErrorToken {
			return false
		}
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
			continue
		}
		tag := z.Token()
		if tag.Data != "meta" {
			continue
		}
		attrs := map[string]string{}
		for _, attr := range tag.Attr {
			attrs[attr.Key] = attr.Val
		}
		if attrs["charset"] != "" {
			return true
		}
		if strings.EqualFold(attrs["http-equiv"], "content-type") {
			_, params, _ := mime.ParseMediaType(attrs["content"])
			if params["charset"] != "" {
				return true
			}
		}
	}
}

// Links are hints only: Fetch validates every URL, DNS answer and redirect.
func publicLinks(body []byte, base *url.URL) []string {
	z := html.NewTokenizer(bytes.NewReader(body))
	links := []string{}
	seen := map[string]bool{}
	for len(links) < 32 {
		kind := z.Next()
		if kind == html.ErrorToken {
			break
		}
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
			continue
		}
		tag := z.Token()
		if tag.Data != "a" {
			continue
		}
		for _, attr := range tag.Attr {
			if attr.Key != "href" {
				continue
			}
			relative, err := url.Parse(attr.Val)
			if err != nil {
				continue
			}
			resolved, err := ValidateURL(base.ResolveReference(relative).String())
			if err == nil && !seen[resolved.String()] {
				seen[resolved.String()] = true
				links = append(links, resolved.String())
			}
		}
	}
	return links
}

func parseHTML(body []byte) (string, string, string) {
	root, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return "", "partial_html_parse", ""
	}
	var groups [4]strings.Builder
	title := ""
	coverage := "partial_html_text_only"
	type frame struct {
		node        *html.Node
		depth, rank int
		hidden      bool
	}
	stack := []frame{{node: root}}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		n := current.node
		if n.Type == html.ElementNode {
			attrs := map[string]string{}
			for _, attr := range n.Attr {
				attrs[attr.Key] = strings.ToLower(attr.Val)
			}
			// Scan the whole document even after text/depth limits, including
			// metadata outside the selected main content and hidden elements.
			if n.Data == "meta" && (attrs["name"] == "robots" || attrs["name"] == "edu-agent" || attrs["http-equiv"] == "cache-control") && restricted(attrs["content"]) {
				return "", "storage_restricted", ""
			}
			if n.Data == "title" && n.Parent != nil && n.Parent.Data == "head" && title == "" && n.FirstChild != nil {
				title = strings.Join(strings.Fields(n.FirstChild.Data), " ")
				if len(title) > 1024 {
					title = title[:1024]
					for !utf8.ValidString(title) {
						title = title[:len(title)-1]
					}
				}
			}
			current.hidden = current.hidden || htmlBoilerplate(n.Data, attrs)
			if !current.hidden {
				switch {
				case htmlContentArea(attrs):
					current.rank = 3
				case n.Data == "article":
					current.rank = max(current.rank, 2)
				case n.Data == "main" || attrs["role"] == "main":
					current.rank = max(current.rank, 1)
				}
			}
		}
		if current.depth > 128 {
			coverage = "partial_depth_limit"
		} else if n.Type == html.TextNode && !current.hidden {
			if group := &groups[current.rank]; group.Len() <= MaxText {
				value := strings.Join(strings.Fields(n.Data), " ")
				if value != "" {
					// Keep one extra rune so Fetch reports the text limit, while
					// continuing to inspect later storage restrictions.
					limit := min(len(value), MaxText+utf8.UTFMax-group.Len())
					for !utf8.ValidString(value[:limit]) {
						limit--
					}
					group.WriteString(value[:limit])
					group.WriteByte(' ')
				}
			}
		}
		for child := n.LastChild; child != nil; child = child.PrevSibling {
			stack = append(stack, frame{node: child, depth: current.depth + 1, rank: current.rank, hidden: current.hidden})
		}
	}
	for rank := len(groups) - 1; rank >= 0; rank-- {
		if text := strings.TrimSpace(groups[rank].String()); text != "" {
			return text, coverage, title
		}
	}
	return "", coverage, title
}

// noindex controls search-engine indexing, not saving an already requested page.
// Continue honoring explicit no-store/noarchive/nosnippet directives.
func restricted(value string) bool {
	for _, directive := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return r == ',' || r == ':' || r == ';' || r == ' ' || r == '\t' || r == '\r' || r == '\n'
	}) {
		name, _, _ := strings.Cut(directive, "=")
		if name == "no-store" || name == "noarchive" || name == "nosnippet" {
			return true
		}
	}
	return false
}

func htmlContentArea(attrs map[string]string) bool {
	for _, value := range strings.Fields(attrs["id"] + " " + attrs["class"]) {
		switch value {
		case "mw-content-text", "mw-parser-output", "article-body", "article-content", "post-content", "entry-content":
			return true
		}
	}
	return false
}

func htmlBoilerplate(name string, attrs map[string]string) bool {
	switch name {
	case "script", "style", "noscript", "head", "template", "svg", "nav", "footer", "aside", "header":
		return true
	}
	if _, ok := attrs["hidden"]; ok {
		return true
	}
	if attrs["aria-hidden"] == "true" {
		return true
	}
	switch attrs["role"] {
	case "navigation", "contentinfo", "banner", "complementary":
		return true
	}
	for _, value := range strings.Fields(attrs["id"] + " " + attrs["class"]) {
		switch value {
		case "toc", "mw-toc", "vector-toc", "table-of-contents", "sidebar", "navigation", "navbox", "footer", "mw-footer", "catlinks", "mw-editsection", "printfooter":
			return true
		}
	}
	return false
}
