// Package apikey is the managed API key format (CP-15, D1, ADR-0039):
// "spw_", an 8-character public prefix, "_", and a 43-character secret,
// all base62. The secret is 32 random bytes (256 bits); the control plane
// stores the prefix and the SHA-256 of the secret, and gateways find a key
// by its prefix and compare the hash in constant time (GW-17, SEC-03).
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"math/big"
	"strings"

	"github.com/ankitbisht23/spillway/internal/secret"
)

const (
	// Start is how every managed key begins.
	Start = "spw_"
	// PrefixLen is the length of the public prefix.
	PrefixLen = 8
	// SecretLen is the length of the secret: 32 bytes in base62, padded.
	SecretLen = 43
	// Len is the length of a whole key.
	Len = len(Start) + PrefixLen + 1 + SecretLen
	// secretBytes is the secret's entropy in bytes (CP-15).
	secretBytes = 32
)

// alphabet is base62: digits, then upper case, then lower case.
const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Pattern is the key grammar as a regular expression; .gitleaks.toml's
// detection rule uses the same one.
const Pattern = `spw_[0-9A-Za-z]{8}_[0-9A-Za-z]{43}`

// Key is a newly generated key. Formatting, logging and JSON show the key
// as "[redacted]"; only Secret.Reveal returns it, for the one answer that
// shows it (ADR-0039).
type Key struct {
	Prefix string
	// Hash is SHA-256 of the secret part, what is stored.
	Hash [sha256.Size]byte
	// Secret is the whole key.
	Secret secret.Value
}

// prefixSpace is 62^8, the number of prefixes.
var prefixSpace = new(big.Int).Exp(big.NewInt(62), big.NewInt(PrefixLen), nil)

// Generate makes a key with crypto/rand: a uniformly random prefix below
// 62^8, and a secret of 32 random bytes written as one big-endian number in
// base62 (ADR-0039).
func Generate() (Key, error) {
	p, err := rand.Int(rand.Reader, prefixSpace)
	if err != nil {
		return Key{}, err
	}
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return Key{}, err
	}
	prefix := base62(p, PrefixLen)
	sec := base62(new(big.Int).SetBytes(raw), SecretLen)
	return Key{Prefix: prefix, Hash: HashSecret(sec), Secret: secret.New(Start + prefix + "_" + sec)}, nil
}

// base62 writes n in base62, left-padded with '0' to width characters.
func base62(n *big.Int, width int) string {
	out := make([]byte, width)
	for i := range out {
		out[i] = '0'
	}
	n = new(big.Int).Set(n)
	base, rem := big.NewInt(62), new(big.Int)
	for i := width - 1; i >= 0 && n.Sign() > 0; i-- {
		n.QuoRem(n, base, rem)
		out[i] = alphabet[rem.Int64()]
	}
	return string(out)
}

// Parse splits a key that follows the grammar into its prefix and secret;
// anything else is not ok.
func Parse(key string) (prefix, sec string, ok bool) {
	if len(key) != Len || !strings.HasPrefix(key, Start) || key[len(Start)+PrefixLen] != '_' {
		return "", "", false
	}
	prefix, sec = key[len(Start):len(Start)+PrefixLen], key[len(Start)+PrefixLen+1:]
	if !IsBase62(prefix) || !IsBase62(sec) {
		return "", "", false
	}
	return prefix, sec, true
}

// IsBase62 reports whether s is made of base62 characters only.
func IsBase62(s string) bool {
	for i := range len(s) {
		c := s[i]
		if !('0' <= c && c <= '9' || 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z') {
			return false
		}
	}
	return true
}

// HashSecret is what is stored of a key: SHA-256 of its secret's ASCII
// characters.
func HashSecret(sec string) [sha256.Size]byte {
	return sha256.Sum256([]byte(sec))
}
