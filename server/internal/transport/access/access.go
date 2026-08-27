package access

import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/edu-agent/edu-agent/server/internal/identity"
)

type credentialContextKey struct{}

func WithCredential(ctx context.Context, credential identity.Credential) context.Context {
	return context.WithValue(ctx, credentialContextKey{}, credential)
}

func CredentialFromContext(ctx context.Context) (identity.Credential, bool) {
	credential, ok := ctx.Value(credentialContextKey{}).(identity.Credential)
	return credential, ok
}

func BearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func ContainsScope(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
