package agentlimits

const UnlimitedToolRounds = 0

// ValidToolRounds accepts the Codex-style unlimited mode (0) and any
// positive user-selected guard. The runtime does not impose a maximum.
func ValidToolRounds(value int) bool {
	return value >= UnlimitedToolRounds
}
