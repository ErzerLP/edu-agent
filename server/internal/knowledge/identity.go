package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

func NormalizePath(value string) (string, error) {
	if value == "" || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "\\") || strings.Contains(value, "\\") {
		return "", &Error{Code: CodeInvalidPath}
	}
	normalized := norm.NFC.String(value)
	if utf8.RuneCountInString(normalized) > MaxPathRunes || len(normalized) > MaxPathBytes {
		return "", &Error{Code: CodeInvalidPath}
	}
	if normalized == "." || normalized == ".." || path.Clean(normalized) != normalized {
		return "", &Error{Code: CodeInvalidPath}
	}
	for _, segment := range strings.Split(normalized, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", &Error{Code: CodeInvalidPath}
		}
		for _, r := range segment {
			if r == 0 || unicode.IsControl(r) {
				return "", &Error{Code: CodeInvalidPath}
			}
		}
	}
	return normalized, nil
}

func FoldPath(value string) string {
	return cases.Fold().String(norm.NFC.String(value))
}

func foldedPath(value string) string {
	return FoldPath(value)
}

func identityTokens(value string) []string {
	value = cases.Fold().String(norm.NFKC.String(value))
	var tokens []string
	var current strings.Builder
	flush := func() {
		if current.Len() != 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	for _, r := range value {
		switch {
		case isCJK(r):
			flush()
			tokens = append(tokens, string(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			current.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return tokens
}

func retrievalTokens(value string) []string {
	base := identityTokens(value)
	result := append([]string(nil), base...)
	var previous string
	for _, token := range base {
		runes := []rune(token)
		if len(runes) == 1 && isCJK(runes[0]) {
			if previous != "" {
				result = append(result, previous+token)
			}
			previous = token
		} else {
			previous = ""
		}
	}
	return uniqueSorted(result)
}

func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

func similarity(oldTokens, newTokens []string, oldHash, newHash string) int {
	if oldHash != "" && oldHash == newHash {
		return 1_000_000
	}
	if len(oldTokens) == 0 || len(newTokens) == 0 {
		return 0
	}
	oldSet := comparisonSet(oldTokens)
	newSet := comparisonSet(newTokens)
	intersection := 0
	union := map[string]struct{}{}
	for token := range oldSet {
		union[token] = struct{}{}
		if _, exists := newSet[token]; exists {
			intersection++
		}
	}
	for token := range newSet {
		union[token] = struct{}{}
	}
	if len(union) == 0 {
		return 0
	}
	return 1_000_000 * intersection / len(union)
}

func comparisonSet(tokens []string) map[string]struct{} {
	result := map[string]struct{}{}
	if len(tokens) < 5 {
		for _, token := range tokens {
			result[token] = struct{}{}
		}
		return result
	}
	for i := 0; i+5 <= len(tokens); i++ {
		result[strings.Join(tokens[i:i+5], "\x1f")] = struct{}{}
	}
	return result
}

func uniqueSorted(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func reviewBasisHash(expectedParent *string, documents []preparedDocument) string {
	var builder strings.Builder
	builder.WriteString("identity-review-basis-v1\n")
	if expectedParent == nil {
		builder.WriteString("-\n")
	} else {
		builder.WriteString(*expectedParent)
		builder.WriteByte('\n')
	}
	builder.WriteString(CanonicalizerVersion)
	builder.WriteByte('\n')
	builder.WriteString(IdentityPolicyVersion)
	builder.WriteByte('\n')
	for _, document := range documents {
		builder.WriteString(document.path)
		builder.WriteByte('|')
		builder.WriteString(reviewDocumentFingerprint(document.inspected))
		builder.WriteByte('\n')
	}
	return sha256Hex([]byte(builder.String()))
}

func reviewDocumentFingerprint(document InspectedDocument) string {
	var builder strings.Builder
	builder.WriteString(document.ExplicitDocumentID)
	builder.WriteByte('|')
	builder.WriteString(document.ExplicitRootNodeID)
	builder.WriteByte('|')
	builder.WriteString(document.SemanticHash)
	builder.WriteByte('\n')
	for _, node := range document.DraftNodes {
		builder.WriteString(node.ExplicitNodeID)
		builder.WriteByte('|')
		builder.WriteString(node.SemanticLocalBodyHash)
		builder.WriteByte('|')
		builder.WriteString(node.Title)
		builder.WriteByte('\n')
	}
	return sha256Hex([]byte(builder.String()))
}

func documentLocator(basis, documentPath string) string {
	return stableLocator("document-locator-v1", basis, documentPath)
}

func nodeLocator(basis, documentPath string, preorder int) string {
	return stableLocator("node-locator-v1", basis, documentPath, fmt.Sprintf("%d", preorder))
}

func stableLocator(parts ...string) string {
	hash := sha256.Sum256([]byte(strings.Join(parts, "\n") + "\n"))
	return hex.EncodeToString(hash[:])
}
