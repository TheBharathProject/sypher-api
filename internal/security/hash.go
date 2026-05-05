// Package security holds salt-and-hash utilities used to record signal
// (e.g. dedup an IP) without storing raw PII.
//
// Why a package instead of inline in each tool: hashing is a cross-cutting
// concern. A tool's handler should call HashIP, not roll its own salted
// SHA-256 — keeps the salt usage consistent and lets us swap the hash
// function later in one place.
package security

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashIP returns hex(SHA-256(salt + ":" + ip)). An empty IP is normalised
// to 0.0.0.0 so the output is always 64 hex chars regardless of input.
//
// Salt comes from the IP_SALT env var; never hardcode it.
func HashIP(salt, ip string) string {
	if ip == "" {
		ip = "0.0.0.0"
	}
	sum := sha256.Sum256([]byte(salt + ":" + ip))
	return hex.EncodeToString(sum[:])
}
