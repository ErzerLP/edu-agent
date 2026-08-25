// Command fakellm runs the programmable strict Chat Completions fixture.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/server/contracttests/fakellm/fixture"
)

func main() {
	address := env("FAKE_LLM_ADDR", "127.0.0.1:18081")
	mode := env("FAKE_LLM_MODE", "success")
	controller := fixture.NewController()
	if err := configureMode(controller, mode); err != nil {
		log.Fatal(err)
	}
	handler := fixture.NewHandler(fixture.HandlerOptions{
		APIKey:       env("FAKE_LLM_API_KEY", "fake-development-key"),
		ControlKey:   env("FAKE_LLM_CONTROL_KEY", "fake-control-key"),
		Controller:   controller,
		TimeoutDelay: time.Duration(envInt("FAKE_LLM_TIMEOUT_MS", 60_000)) * time.Millisecond,
	})
	log.Printf("fake LLM listening on %s in %s mode", address, mode)
	log.Fatal(http.ListenAndServe(address, handler))
}

func configureMode(controller *fixture.Controller, mode string) error {
	if mode == "success" || mode == "accepted" {
		return nil
	}
	var scenario fixture.Scenario
	switch mode {
	case "provisional":
		scenario = fixture.Scenario{Kind: fixture.ScenarioProvisional}
	case "invalid-json":
		scenario = fixture.Scenario{Kind: fixture.ScenarioMalformedEnvelope}
	case "malformed":
		scenario = fixture.Scenario{Kind: fixture.ScenarioMalformed}
	case "schema-mismatch":
		scenario = fixture.Scenario{Kind: fixture.ScenarioSchemaMismatch}
	case "unauthorized":
		scenario = fixture.Scenario{Kind: fixture.ScenarioUnauthorized}
	case "rate-limited":
		scenario = fixture.Scenario{Kind: fixture.ScenarioRateLimited}
	case "server-error":
		scenario = fixture.Scenario{Kind: fixture.ScenarioHTTPError, StatusCode: http.StatusBadGateway}
	case "timeout":
		scenario = fixture.Scenario{Kind: fixture.ScenarioTimeout}
	case "no-native-schema":
		return controller.Configure(fixture.KindCapabilityProbe, fixture.Scenario{Kind: fixture.ScenarioNoNativeSchema})
	default:
		if strings.HasPrefix(mode, "risk:") {
			scenario = fixture.Scenario{Kind: fixture.ScenarioRisk, RiskFlag: strings.TrimPrefix(mode, "risk:")}
			break
		}
		return fmt.Errorf("unknown FAKE_LLM_MODE %q", mode)
	}
	for _, kind := range fixture.AllRequestKinds() {
		if err := controller.Configure(kind, scenario); err != nil {
			return err
		}
	}
	return nil
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value := env(name, strconv.Itoa(fallback))
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		log.Fatalf("%s must be a positive integer", name)
	}
	return parsed
}
