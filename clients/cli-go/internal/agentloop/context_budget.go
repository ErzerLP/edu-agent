package agentloop

import (
	"encoding/json"
	"math"
	"sync"
	"unicode"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

// TokenEstimator conservatively estimates complete OpenAI-compatible requests
// and can be calibrated from optional provider prompt usage.
type TokenEstimator interface {
	EstimateText(string) int
	EstimateRequest(modelclient.Request) int
	ObserveActual(int, modelclient.Usage)
}

type ConservativeTokenEstimator struct {
	mu         sync.RWMutex
	correction float64
}

func NewTokenEstimator() *ConservativeTokenEstimator {
	return &ConservativeTokenEstimator{correction: 1}
}

func (e *ConservativeTokenEstimator) EstimateText(value string) int {
	ascii, cjk, other := 0, 0, 0
	for _, current := range value {
		switch {
		case current <= unicode.MaxASCII:
			ascii++
		case unicode.In(current, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul):
			cjk++
		default:
			other++
		}
	}
	// ASCII prose is commonly near four characters/token, but JSON and code are
	// denser. Three characters/token plus one token per CJK rune is deliberately
	// conservative before provider calibration is available.
	base := divideRoundUp(ascii, 3) + cjk + divideRoundUp(other, 2)
	if value != "" && base == 0 {
		base = 1
	}
	e.mu.RLock()
	correction := e.correction
	e.mu.RUnlock()
	return int(math.Ceil(float64(base) * correction))
}

func (e *ConservativeTokenEstimator) EstimateRequest(request modelclient.Request) int {
	// Request and message envelopes vary by provider. These constants are
	// intentionally biased upward and tool schemas are charged in full.
	total := 8
	for _, message := range request.Messages {
		total += 6 + e.EstimateText(message.Role) + e.EstimateText(message.Content) + e.EstimateText(message.ToolCallID)
		for _, call := range message.ToolCalls {
			total += 10 + e.EstimateText(call.ID) + e.EstimateText(call.Type) + e.EstimateText(call.Function.Name) + e.EstimateText(call.Function.Arguments)
		}
	}
	for _, tool := range request.Tools {
		total += 14 + e.EstimateText(tool.Type) + e.EstimateText(tool.Function.Name) + e.EstimateText(tool.Function.Description)
		if len(tool.Function.Parameters) != 0 {
			var compact any
			if json.Unmarshal(tool.Function.Parameters, &compact) == nil {
				if data, err := json.Marshal(compact); err == nil {
					total += e.EstimateText(string(data))
					continue
				}
			}
			total += e.EstimateText(string(tool.Function.Parameters))
		}
	}
	return total
}

func (e *ConservativeTokenEstimator) ObserveActual(estimatedInput int, usage modelclient.Usage) {
	actual := usage.PromptTokens
	if estimatedInput <= 0 || actual <= 0 {
		return
	}
	ratio := float64(actual) / float64(estimatedInput)
	// Ignore implausible provider counters instead of allowing a single bad
	// response to poison all later requests.
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0.5 || ratio > 2.5 {
		return
	}
	e.mu.Lock()
	if e.correction <= 0 || math.IsNaN(e.correction) || math.IsInf(e.correction, 0) {
		e.correction = 1
	}
	const alpha = 0.2
	e.correction *= (1 - alpha) + alpha*ratio
	e.correction = math.Max(0.75, math.Min(2.5, e.correction))
	e.mu.Unlock()
}

func (e *ConservativeTokenEstimator) Calibration() float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.correction
}

func divideRoundUp(value, divisor int) int {
	if value <= 0 {
		return 0
	}
	return (value + divisor - 1) / divisor
}
