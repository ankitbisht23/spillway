package apikey

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Keys in these tests are made at run time: a literal key would trip the
// gitleaks rule (ADR-0039).

func TestGenerateFormat(t *testing.T) {
	re := regexp.MustCompile("^" + Pattern + "$")
	prefixes, secrets := map[string]bool{}, map[string]bool{}
	for range 2000 {
		k, err := Generate()
		if err != nil {
			t.Fatal(err)
		}
		key := k.Secret.Reveal()
		if len(key) != Len || Len != 56 || !re.MatchString(key) {
			t.Fatalf("key of length %d does not follow the grammar", len(key))
		}
		prefix, sec, ok := Parse(key)
		if !ok || prefix != k.Prefix || len(sec) != SecretLen {
			t.Fatalf("Parse = %q, ok %v; want the prefix %q and a %d-character secret", prefix, ok, k.Prefix, SecretLen)
		}
		if k.Hash != sha256.Sum256([]byte(sec)) || k.Hash != HashSecret(sec) {
			t.Fatal("Hash is not SHA-256 of the secret part")
		}
		if prefixes[prefix] || secrets[sec] {
			t.Fatal("a prefix or secret repeated in 2,000 keys")
		}
		prefixes[prefix], secrets[sec] = true, true
	}
}

// TestGenerateEntropy checks that the secret is 256 random bits (CP-15):
// it decodes to a number below 2^256, and each of its 256 bits is set in
// about half of 2,000 keys. The bounds are over 13 standard deviations
// wide, so a correct generator never fails them.
func TestGenerateEntropy(t *testing.T) {
	const n = 2000
	limit := new(big.Int).Lsh(big.NewInt(1), 256)
	var ones [256]int
	for range n {
		k, err := Generate()
		if err != nil {
			t.Fatal(err)
		}
		_, sec, _ := Parse(k.Secret.Reveal())
		v := decode(t, sec)
		if v.Cmp(limit) >= 0 {
			t.Fatal("the secret is not below 2^256")
		}
		for i := range 256 {
			ones[i] += int(v.Bit(i))
		}
	}
	for i, c := range ones {
		if c < n*35/100 || c > n*65/100 {
			t.Errorf("bit %d set in %d of %d keys", i, c, n)
		}
	}
}

func decode(t *testing.T, s string) *big.Int {
	t.Helper()
	v := new(big.Int)
	for _, c := range s {
		i := strings.IndexRune(alphabet, c)
		if i < 0 {
			t.Fatalf("%q is not base62", c)
		}
		v.Mul(v, big.NewInt(62)).Add(v, big.NewInt(int64(i)))
	}
	return v
}

func TestBase62Padding(t *testing.T) {
	if got := base62(big.NewInt(0), 8); got != "00000000" {
		t.Errorf("base62(0) = %q", got)
	}
	if got := base62(big.NewInt(62*62+1), 4); got != "0101" {
		t.Errorf("base62(3845) = %q", got)
	}
	max := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	if got := base62(max, SecretLen); len(got) != SecretLen || decode(t, got).Cmp(max) != 0 {
		t.Errorf("2^256-1 does not round-trip in %d characters", SecretLen)
	}
}

func TestParse(t *testing.T) {
	p, s := strings.Repeat("a", PrefixLen), strings.Repeat("B", SecretLen)
	if prefix, sec, ok := Parse(Start + p + "_" + s); !ok || prefix != p || sec != s {
		t.Errorf("Parse of a valid key = %q, %q, %v", prefix, sec, ok)
	}
	for _, bad := range []string{
		"",
		Start + p + "_" + s + "x",
		Start + p + "_" + s[1:],
		Start + p + "-" + s,
		"spx_" + p + "_" + s,
		Start + "aaaaaaa!" + "_" + s,
		Start + p + "_" + s[1:] + "+",
		Start + p + "_" + s[1:] + "é"[:1],
	} {
		if _, _, ok := Parse(bad); ok {
			t.Errorf("Parse accepted a malformed key of length %d", len(bad))
		}
	}
}

// TestKeyNeverPrinted: the whole key shows only through Reveal.
func TestKeyNeverPrinted(t *testing.T) {
	k, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	slog.New(slog.NewJSONHandler(&log, nil)).Info("issued", "key", k, "secret", k.Secret)
	j, _ := json.Marshal(k)
	_, sec, _ := Parse(k.Secret.Reveal())
	for _, out := range []string{fmt.Sprint(k), fmt.Sprintf("%+v %#v", k, k), log.String(), string(j)} {
		if strings.Contains(out, sec) {
			t.Errorf("the secret appears in %q", out)
		}
	}
}

// TestGitleaksRule checks that .gitleaks.toml detects the key grammar
// (ADR-0039): a detection rule with Pattern, and the allowlist still only
// the one dev value.
func TestGitleaksRule(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".gitleaks.toml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "[[rules]]") || !strings.Contains(s, "regex = '''"+Pattern+"'''") {
		t.Errorf(".gitleaks.toml has no rule with regex %s", Pattern)
	}
	if strings.Count(s, "regexes = ") != 1 || !strings.Contains(s, "regexes = ['''^dev-only-not-secret$''']") {
		t.Error(".gitleaks.toml's allowlist changed")
	}
}
