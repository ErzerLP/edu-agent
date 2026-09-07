package agentloop

// The complete relocation fact and the current attempt's replay decision are
// retained separately from preview prose. Paths and versions are never sliced.
func restoreReceiptProjection(object map[string]any) map[string]any {
	return preserveFields(object, "file_effect", "operation", "path", "source", "destination", "entry_type", "publication_outcome", "operation_outcome", "error", "code", "replay")
}
