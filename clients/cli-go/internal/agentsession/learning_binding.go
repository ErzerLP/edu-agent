package agentsession

// v9 不允许学习绑定字段；旧会话只映射固定默认区。
type recordPayloadV9 struct {
	recordPayloadV1
	FileReceipts []FileReceipt `json:"file_receipts,omitempty"`
}
