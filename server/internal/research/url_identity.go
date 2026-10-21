package research

import (
	"net/url"
	"strings"
)

// NormalizeURL removes only fragment/default port and explicit tracking keys.
// Semantic query parameters and unverified canonical tags never change identity.
func NormalizeURL(raw string) (string, error) {
	u, err := ValidateURL(raw)
	if err != nil {
		return "", err
	}
	u.Fragment = ""
	u.RawFragment = ""
	u.Host = strings.ToLower(u.Host)
	if u.Scheme == "https" && u.Port() == "443" || u.Scheme == "http" && u.Port() == "80" {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	if u.Path == "" {
		u.Path = "/"
	}
	parts := strings.Split(u.RawQuery, "&")
	query := make([]string, 0, len(parts))
	for _, part := range parts {
		key, _, _ := strings.Cut(part, "=")
		decoded, e := url.QueryUnescape(key)
		lower := strings.ToLower(decoded)
		if e == nil && (strings.HasPrefix(lower, "utm_") || lower == "gclid" || lower == "fbclid" || lower == "msclkid") {
			continue
		}
		query = append(query, part)
	}
	u.RawQuery = strings.Join(query, "&")
	return u.String(), nil
}
