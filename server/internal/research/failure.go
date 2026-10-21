package research

// FailureCategory groups detailed fetch/parser reasons without discarding them.
// HTTP failures retain the status (http_403, http_404, http_429, ...).
func FailureCategory(reason string) string {
	switch reason {
	case "body_limit", "decoded_limit", "pdf_file_limit", "pdf_page_limit":
		return "too_large"
	case "unsupported_format":
		return "unsupported_type"
	case "network_policy_rejected":
		return "blocked_policy"
	case "pdf_timeout":
		return "timeout"
	case "pdf_no_text":
		return "parse_empty"
	case "pdf_invalid", "pdf_invalid_or_resource_limit":
		return "pdf_parse_failed"
	case "":
		return "parse_empty"
	default:
		return reason
	}
}
