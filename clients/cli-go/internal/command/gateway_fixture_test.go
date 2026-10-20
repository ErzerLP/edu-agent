package command

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
)

// This stateless HTTP fixture supplies only safe gateway configuration to tests
// that isolate local tools with a model fake. Connection tests own their server.
var toolGatewayFixture = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/model-gateway/config" {
		http.Error(w, "unexpected model call in tool-only fixture", 500)
		return
	}
	fmt.Fprint(w, `{"protocol_version":1,"invoke_allowed":true,"available":true,"privacy_generation":7,"models":[{"target":"teaching","display_name":"教学模型","model_name":"test-model","configuration_revision":"c1","destination_revision":"d1","context_tokens":32768,"output_tokens":2048}],"limits":{"idle_timeout_seconds":60,"request_timeout_seconds":900}}`)
}))

type toolGatewayFixtureTransport struct{}

func (toolGatewayFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	target, _ := url.Parse(toolGatewayFixture.URL + r.URL.Path)
	copy.URL = target
	return http.DefaultTransport.RoundTrip(copy)
}
