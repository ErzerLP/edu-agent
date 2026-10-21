package research

import (
	"github.com/edu-agent/edu-agent/server/internal/integrations/websearch"
	"net/http"
)

// SearchClient is a trusted host adapter factory, never supplied by a model.
type SearchClient func(func(http.RoundTripper) http.RoundTripper) (websearch.Adapter, string, error)
