package agentlimits

const (
	MaxToolCallArgumentsBytes     = 8 << 10
	MaxFileMutationArgumentsBytes = 64 << 10
	MaxToolCallArgumentsTotal     = 128 << 10
)

// ToolArgumentsBytes limits the whole JSON argument object, including escaping
// and metadata. Unknown tools keep the smaller limit, not the file-write limit.
func ToolArgumentsBytes(name string) int {
	switch name {
	case "write", "edit":
		return MaxFileMutationArgumentsBytes
	default:
		return MaxToolCallArgumentsBytes
	}
}
