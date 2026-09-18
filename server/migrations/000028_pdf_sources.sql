-- PDF 原件与提取报告复用正文的不可变及隐私代次触发器，不建立旁路缓存。
ALTER TABLE knowledge_document_payloads
    ADD COLUMN pdf_original BYTEA,
    ADD COLUMN pdf_metadata JSONB;

ALTER TABLE knowledge_document_payloads ADD CONSTRAINT knowledge_pdf_payload_bounds
    CHECK ((pdf_original IS NULL AND pdf_metadata IS NULL) OR
           (pdf_original IS NOT NULL AND octet_length(pdf_original) BETWEEN 1 AND 4194304 AND
            pdf_metadata IS NOT NULL AND jsonb_typeof(pdf_metadata) = 'object'));
