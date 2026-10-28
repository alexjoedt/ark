package ark

import (
	"crypto/rand"
	"time"
)

// generateID creates a sortable, unique snapshot ID.
// Format: 10-char timestamp prefix (Crockford base32, ms precision) +
// 16-char random suffix (80 bits of entropy). Total length: 26 characters.
func generateID(timestamp time.Time) string {
	tsMs := uint64(timestamp.UnixNano() / int64(time.Millisecond))
	tsPart := encodeTimestampBase32(tsMs, 10)

	randomBytes := make([]byte, 10)
	rand.Read(randomBytes)
	return tsPart + encodeBase32(randomBytes)
}

func encodeTimestampBase32(timestamp uint64, length int) string {
	const chars = "0123456789abcdefghjkmnpqrstvwxyz"
	result := make([]byte, length)
	for i := length - 1; i >= 0; i-- {
		result[i] = chars[timestamp%32]
		timestamp /= 32
	}
	return string(result)
}

func encodeBase32(data []byte) string {
	const chars = "0123456789abcdefghjkmnpqrstvwxyz"
	if len(data) == 0 {
		return ""
	}
	outputLen := (len(data)*8 + 4) / 5
	result := make([]byte, outputLen)
	var bits uint64
	bitsLen := 0
	idx := 0
	for _, b := range data {
		bits = (bits << 8) | uint64(b)
		bitsLen += 8
		for bitsLen >= 5 {
			result[idx] = chars[(bits>>(bitsLen-5))&0x1F]
			idx++
			bitsLen -= 5
		}
	}
	if bitsLen > 0 {
		result[idx] = chars[(bits<<uint(5-bitsLen))&0x1F]
	}
	return string(result)
}
