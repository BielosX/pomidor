package util

import (
	"crypto/sha256"
	"encoding/hex"
)

func Sha256String(str string) string {
	sum := sha256.Sum256([]byte(str))
	return hex.EncodeToString(sum[:])
}
