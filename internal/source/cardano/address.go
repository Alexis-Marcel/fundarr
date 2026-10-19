package cardano

import (
	"errors"
	"strings"

	"github.com/btcsuite/btcd/btcutil/bech32"
)

// Shelley address headers (CIP-19): the high nibble is the type, the low one
// the network (1 = mainnet).
const (
	mainnet       = 0x01
	stakeKeyHdr   = 0xe1 // stake address, key hash, mainnet
	stakeShHdr    = 0xf1 // stake address, script hash, mainnet
	hashLen       = 28
	errNoStakeMsg = "give a receive address that includes staking (addr1q…) or the stake address (stake1…)"
)

// decode reads a bech32 string of any length (Cardano addresses exceed the
// 90 characters of BIP173), refusing bech32m.
func decode(s string) (string, []byte, error) {
	hrp, data, v, err := bech32.DecodeNoLimitWithVersion(s)
	if err != nil {
		return "", nil, err
	}
	if v != bech32.Version0 {
		return "", nil, errors.New("not bech32")
	}
	b, err := bech32.ConvertBits(data, 5, 8, false)
	if err != nil {
		return "", nil, err
	}
	return hrp, b, nil
}

// stakeAddress returns the stake address (stake1…) behind what the user
// pasted: a base address addr1… carrying a stake credential, or the stake
// address itself.
func stakeAddress(s string) (string, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "Ae2") || strings.HasPrefix(s, "DdzFF") {
		return "", errors.New("cardano: this is an old Byron address, without staking: " + errNoStakeMsg)
	}
	hrp, b, err := decode(s)
	if err != nil {
		return "", errors.New("cardano: not a valid Cardano address (a character was mistyped?): " + errNoStakeMsg)
	}
	if strings.HasSuffix(hrp, "_test") {
		return "", errors.New("cardano: this is a testnet address, only mainnet is read")
	}
	if len(b) == 0 || b[0]&0x0f != mainnet {
		return "", errors.New("cardano: not a mainnet address")
	}
	typ := b[0] >> 4
	switch hrp {
	case "stake":
		if (typ != 14 && typ != 15) || len(b) != 1+hashLen {
			return "", errors.New("cardano: malformed stake address")
		}
		return s, nil
	case "addr":
		if typ > 3 {
			// Pointer (4, 5) and enterprise (6, 7) addresses carry no
			// stake key hash; Byron ones are not bech32.
			return "", errors.New("cardano: this address has no staking part: " + errNoStakeMsg)
		}
		if len(b) != 1+2*hashLen {
			return "", errors.New("cardano: malformed base address")
		}
		hdr := byte(stakeKeyHdr)
		if typ == 2 || typ == 3 {
			hdr = stakeShHdr // the stake credential is a script hash
		}
		data, err := bech32.ConvertBits(append([]byte{hdr}, b[1+hashLen:]...), 8, 5, true)
		if err != nil {
			return "", err
		}
		return bech32.Encode("stake", data)
	}
	return "", errors.New("cardano: unknown address kind " + hrp + ": " + errNoStakeMsg)
}

// Valid reports whether s is a mainnet base address with a stake part
// (addr1…) or a stake address (stake1…).
func Valid(s string) bool {
	_, err := stakeAddress(s)
	return err == nil
}
