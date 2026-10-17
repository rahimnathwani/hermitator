// Package ids generates the identifiers and opaque tokens hermitator hands out.
package ids

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

var lower32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// New returns a random 10 character lowercase identifier (50 bits).
//
// Agent IDs end up in network interface names, which Linux caps at 15
// bytes, so identifiers are kept deliberately short.
func New() string {
	var b [7]byte
	rand.Read(b[:])
	return lower32.EncodeToString(b[:])[:10]
}

// Token returns a random 256-bit token suitable for session cookies.
func Token() string {
	var b [32]byte
	rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// Hash returns the hex SHA-256 of a token. Only hashes are stored, so a
// leaked database does not leak live sessions.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// UUID returns a random RFC 4122 version 4 UUID. Both harnesses require
// session IDs in this format.
func UUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// linkAlphabet avoids characters that are easy to confuse when read off a
// screen and typed on a phone keyboard.
const linkAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// LinkCode returns a short code for the /link chat command.
func LinkCode() string {
	var b [5]byte
	rand.Read(b[:])
	var sb strings.Builder
	for _, c := range b {
		sb.WriteByte(linkAlphabet[int(c)%len(linkAlphabet)])
	}
	return sb.String()
}
