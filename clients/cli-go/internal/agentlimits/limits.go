package agentlimits

const (
	UnlimitedToolRounds   = 0
	DefaultContextWindow  = 272000
	MaxOutputTokens       = 128000
	MaxAssistantTextBytes = 1 << 20
)

// ValidMaxTokens validates an explicit output ceiling. Zero is resolved by callers.
func ValidMaxTokens(value int) bool { return value > 0 && value <= MaxOutputTokens }

// ValidToolRounds accepts the Codex-style unlimited mode (0) and any
// positive user-selected guard. The runtime does not impose a maximum.
func ValidToolRounds(value int) bool {
	return value >= UnlimitedToolRounds
}
