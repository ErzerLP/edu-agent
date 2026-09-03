package agentlimits

const (
	MinToolRounds           = 1
	MaxToolRounds           = 60
	MaxToolCallsPerResponse = 4
)

func ValidToolRounds(value int) bool {
	return value >= MinToolRounds && value <= MaxToolRounds
}

func MaxToolCalls(toolRounds int) int {
	if !ValidToolRounds(toolRounds) {
		return 0
	}
	return toolRounds * MaxToolCallsPerResponse
}
