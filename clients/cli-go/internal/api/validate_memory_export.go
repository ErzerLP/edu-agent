package api

import (
	"errors"
	"strings"
	"unicode/utf8"
)

func validateMemoryExportPage(value MemoryExportPage) error {
	if value.Items == nil || value.ReasonCodes == nil || value.ReadGeneration.LearnerGeneration < 1 || value.ReadGeneration.MemoryGeneration < 1 {
		return errors.New("memory export page is incomplete")
	}
	for _, code := range value.ReasonCodes {
		if strings.TrimSpace(code) == "" || len(code) > 500 {
			return errors.New("memory export reason code is invalid")
		}
	}
	for _, item := range value.Items {
		if err := validateMemoryExportItem(item); err != nil {
			return err
		}
	}
	return nil
}

func validateMemoryExportItem(value MemoryExportItem) error {
	record := value.Record
	if !validLearningUUID(record.LogicalMemoryID) || !validLearningUUID(record.RecordRevisionID) ||
		record.Revision < 1 || record.RecordGeneration < 1 || record.LearnerGeneration < 1 ||
		!validLearningUUID(record.CandidateID) || !optionalLearningUUID(record.PreviousRecordRevisionID) ||
		!validMemoryExternalURI(record.ExternalURI) || !validSHA256(record.ExternalURISHA256) ||
		!optionalLearningUUID(record.ExternalNodeID) || record.ExternalMemoryID < 0 || !validSHA256(record.ContentSHA256) ||
		!validMemoryRecordStatus(record.Status) || !validLearningUUID(record.DeliveryID) ||
		!validLearningUUID(record.ReceiptID) || record.CreatedAt.IsZero() {
		return errors.New("memory export record is incomplete")
	}
	receipt := value.Receipt
	if !validLearningUUID(receipt.ID) || !validLearningUUID(receipt.DeliveryID) || receipt.Version < 1 ||
		!validMemoryReceiptStatus(receipt.Status) || strings.TrimSpace(receipt.Reason) == "" || len(receipt.Reason) > 1000 ||
		strings.TrimSpace(receipt.VerificationMethod) == "" || len(receipt.VerificationMethod) > 500 ||
		(receipt.EvidenceDigest != "" && !validSHA256(receipt.EvidenceDigest)) || receipt.CreatedAt.IsZero() {
		return errors.New("memory export receipt is incomplete")
	}
	if receipt.DeliveryID != record.DeliveryID || receipt.ID != record.ReceiptID || !validMemoryDeliveryStatus(value.DeliveryStatus) {
		return errors.New("memory export delivery identity is inconsistent")
	}
	if !validMemoryContentStatus(value.ContentStatus) || !utf8.ValidString(value.Content) || len([]byte(value.Content)) > 262144 || len([]rune(value.Content)) > 32000 {
		return errors.New("memory export content is invalid")
	}
	if value.ContentStatus == "available" && strings.TrimSpace(value.Content) == "" {
		return errors.New("available memory export content is missing")
	}
	return nil
}

func optionalLearningUUID(value string) bool { return value == "" || validLearningUUID(value) }

func validMemoryExternalURI(value string) bool {
	const prefix = "nocturne://core/edu-agent/"
	return strings.HasPrefix(value, prefix) && validLearningUUID(strings.TrimPrefix(value, prefix))
}

func validMemoryRecordStatus(value string) bool {
	switch value {
	case "queued", "applied", "permanently_rejected", "superseded", "delete_pending", "deleted":
		return true
	default:
		return false
	}
}

func validMemoryReceiptStatus(value string) bool {
	switch value {
	case "pending", "succeeded", "partial", "failed", "unknown", "not_applicable", "unsupported":
		return true
	default:
		return false
	}
}

func validMemoryDeliveryStatus(value string) bool {
	return value == "queued" || value == "applied" || value == "rejected"
}

func validMemoryContentStatus(value string) bool {
	switch value {
	case "available", "degraded", "unavailable", "redacted":
		return true
	default:
		return false
	}
}
