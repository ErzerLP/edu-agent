package observability

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"strings"
)

const redacted = "[REDACTED]"

var (
	bearerPattern = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/-]+=*`)
	urlPassword   = regexp.MustCompile(`(postgres(?:ql)?://[^:/\s]+:)[^@\s]+(@)`)
	secretKeys    = map[string]struct{}{
		"authorization": {}, "api_key": {}, "apikey": {}, "model_api_key": {},
		"token": {}, "access_token": {}, "device_token": {}, "pairing_code": {},
		"code": {}, "database_url": {}, "password": {}, "secret": {},
	}
)

func NewJSON(w io.Writer, level slog.Leveler) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if _, sensitive := secretKeys[strings.ToLower(attr.Key)]; sensitive {
				return slog.String(attr.Key, redacted)
			}
			if attr.Value.Kind() == slog.KindString {
				return slog.String(attr.Key, RedactString(attr.Value.String()))
			}
			if attr.Value.Kind() == slog.KindAny {
				if err, ok := attr.Value.Any().(error); ok {
					return slog.String(attr.Key, RedactString(err.Error()))
				}
			}
			return attr
		},
	}))
}

func RedactString(value string) string {
	value = bearerPattern.ReplaceAllString(value, "Bearer "+redacted)
	return urlPassword.ReplaceAllString(value, `${1}`+redacted+`${2}`)
}

func Error(logger *slog.Logger, ctx context.Context, message string, err error, attrs ...any) {
	args := []any{"error", RedactString(err.Error())}
	args = append(args, attrs...)
	logger.ErrorContext(ctx, message, args...)
}
