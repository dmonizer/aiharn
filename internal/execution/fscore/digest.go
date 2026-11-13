package fscore

import (
	"crypto/sha256"
	"encoding/hex"
)

// digestHex returns the lowercase hex SHA-256 of content.
func digestHex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
