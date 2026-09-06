package workspace

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

// Freeze the bytes actually parsed, rather than trusting sub-tick metadata
// changes to always identify a different file body. Validation may reread these
// bounded files; it does not charge that safety I/O as newly scanned content.
func (q *workspaceQuery) rememberFileHash(path string, data []byte) error {
	digest := sha256.Sum256(data)
	hash := "sha256:" + hex.EncodeToString(digest[:])
	if previous, exists := q.hashes[path]; exists {
		if previous != hash {
			q.fatal = securefile.ErrChanged
			return q.fatal
		}
		return nil
	}
	if !q.charge(int64(len(path)+128), 0) {
		return operationFailure(CodeQueryCapacity, "query retention is full")
	}
	if q.hashes == nil {
		q.hashes = make(map[string]string)
	}
	q.hashes[path] = hash
	return nil
}
