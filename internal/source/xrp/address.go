package xrp

import (
	"bytes"
	"crypto/sha256"
	"math/big"
	"strings"
)

// alphabet is the XRP Ledger's base58 alphabet (not Bitcoin's).
const alphabet = "rpshnaf39wBUDNEGHJKLM4PQRST7VWXYZ2bcdeCg65jkm8oFqi1tuvAxyz"

// Valid reports whether s is a classic XRP Ledger address: r…, base58check
// with the Ripple alphabet, version 0x00, a 20-byte account id and a correct
// checksum. X-addresses (X…) are not accepted.
func Valid(s string) bool {
	if len(s) < 25 || len(s) > 35 || s[0] != 'r' {
		return false
	}
	b, ok := decode58(s)
	if !ok || len(b) != 25 || b[0] != 0x00 {
		return false
	}
	h := sha256.Sum256(b[:21])
	h = sha256.Sum256(h[:])
	return bytes.Equal(h[:4], b[21:])
}

// decode58 decodes base58 with the Ripple alphabet; leading 'r's are
// leading zero bytes.
func decode58(s string) ([]byte, bool) {
	n := new(big.Int)
	radix := big.NewInt(58)
	for _, c := range s {
		i := strings.IndexRune(alphabet, c)
		if i < 0 {
			return nil, false
		}
		n.Mul(n, radix)
		n.Add(n, big.NewInt(int64(i)))
	}
	zeros := 0
	for zeros < len(s) && s[zeros] == alphabet[0] {
		zeros++
	}
	return append(make([]byte, zeros), n.Bytes()...), true
}
