package bitcoin

import (
	"strings"

	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/base58"
	"github.com/btcsuite/btcd/chaincfg"
)

// Coin is a blockchain built like Bitcoin (UTXO, BIP32 keys): Litecoin,
// Dogecoin and Bitcoin Cash differ only in how keys and addresses are
// written, and in which public servers index them.
type Coin struct {
	Name   string // "Bitcoin"
	Symbol string // "BTC"
	// params encodes addresses (version bytes, bech32 prefix).
	params *chaincfg.Params
	// cashaddr: Bitcoin Cash writes addresses as bitcoincash:q…
	cashaddr bool
	// keys: extended public key versions accepted, and the address formats
	// each one scans (a version that does not say its format scans all).
	keys map[[4]byte][]scriptType
	// servers: public APIs tried in order when one stops answering.
	servers []server
}

// server is one public API: an Esplora (mempool, electrs) or a Blockbook
// (Trezor's indexer); both say the same things in other words.
type server struct {
	url       string
	blockbook bool
}

func (c Coin) asset() model.Asset { return model.Asset{Kind: model.Crypto, Symbol: c.Symbol} }

var (
	verXpub = [4]byte{0x04, 0x88, 0xb2, 0x1e}
	verYpub = [4]byte{0x04, 0x9d, 0x7c, 0xb2}
	verZpub = [4]byte{0x04, 0xb2, 0x47, 0x46}
	verLtub = [4]byte{0x01, 0x9d, 0xa4, 0x62} // Litecoin, what Ledger Live shows for every account type
	verMtub = [4]byte{0x01, 0xb2, 0x6e, 0xf6} // Litecoin nested segwit
	verDgub = [4]byte{0x02, 0xfa, 0xca, 0xfd} // Dogecoin
)

var (
	Bitcoin = Coin{Name: "Bitcoin", Symbol: "BTC", params: &chaincfg.MainNetParams,
		keys: map[[4]byte][]scriptType{
			verXpub: {p2wpkh, p2tr, p2shP2wpkh, p2pkh},
			verYpub: {p2shP2wpkh},
			verZpub: {p2wpkh},
		},
		servers: []server{{url: "https://mempool.space/api"}, {url: "https://blockstream.info/api"}}}

	Litecoin = Coin{Name: "Litecoin", Symbol: "LTC",
		params: &chaincfg.Params{Name: "litecoin", PubKeyHashAddrID: 0x30, ScriptHashAddrID: 0x32, Bech32HRPSegwit: "ltc"},
		keys: map[[4]byte][]scriptType{
			verLtub: {p2wpkh, p2shP2wpkh, p2pkh},
			verXpub: {p2wpkh, p2shP2wpkh, p2pkh},
			verMtub: {p2shP2wpkh},
			verZpub: {p2wpkh},
		},
		servers: []server{{url: "https://litecoinspace.org/api"}, {url: "https://litecoin.atomicwallet.io/api/v2", blockbook: true}}}

	Dogecoin = Coin{Name: "Dogecoin", Symbol: "DOGE",
		params:  &chaincfg.Params{Name: "dogecoin", PubKeyHashAddrID: 0x1e, ScriptHashAddrID: 0x16},
		keys:    map[[4]byte][]scriptType{verDgub: {p2pkh}, verXpub: {p2pkh}},
		servers: []server{{url: "https://dogecoin.atomicwallet.io/api/v2", blockbook: true}}}

	BitcoinCash = Coin{Name: "Bitcoin Cash", Symbol: "BCH", params: &chaincfg.MainNetParams, cashaddr: true,
		keys:    map[[4]byte][]scriptType{verXpub: {p2pkh}},
		servers: []server{{url: "https://bchblockexplorer.com/api/v2", blockbook: true}}}
)

// ValidAddress reports whether a is one address of the coin (legacy,
// nested or native segwit, taproot). Bitcoin Cash addresses are not read
// alone: its wallets always give an extended public key.
func (c Coin) ValidAddress(a string) bool {
	if c.cashaddr {
		return false
	}
	if c.params.Bech32HRPSegwit != "" && strings.HasPrefix(strings.ToLower(a), c.params.Bech32HRPSegwit+"1") {
		d, err := btcutil.DecodeAddress(a, c.params)
		return err == nil && d.IsForNet(c.params)
	}
	raw, ver, err := base58.CheckDecode(a)
	return err == nil && len(raw) == 20 && (ver == c.params.PubKeyHashAddrID || ver == c.params.ScriptHashAddrID)
}

// cashAddr writes a pay-to-pubkey-hash in Bitcoin Cash's format.
func cashAddr(hash []byte) string {
	const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
	const prefix = "bitcoincash"
	// Version byte 0 (P2PKH, 160 bits), then the hash, regrouped in 5 bits.
	payload := convert(append([]byte{0}, hash...))
	values := make([]byte, 0, len(prefix)+1+len(payload)+8)
	for i := 0; i < len(prefix); i++ {
		values = append(values, prefix[i]&0x1f)
	}
	values = append(values, 0)
	values = append(values, payload...)
	values = append(values, make([]byte, 8)...)
	mod := polymod(values) ^ 1
	out := []byte(prefix + ":")
	for _, v := range payload {
		out = append(out, charset[v])
	}
	for i := 0; i < 8; i++ {
		out = append(out, charset[(mod>>(5*(7-i)))&0x1f])
	}
	return string(out)
}

func convert(data []byte) []byte {
	var out []byte
	acc, bits := 0, 0
	for _, b := range data {
		acc = acc<<8 | int(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out = append(out, byte(acc>>bits)&0x1f)
		}
	}
	if bits > 0 {
		out = append(out, byte(acc<<(5-bits))&0x1f)
	}
	return out
}

func polymod(v []byte) uint64 {
	c := uint64(1)
	for _, d := range v {
		c0 := byte(c >> 35)
		c = (c&0x07ffffffff)<<5 ^ uint64(d)
		for i, g := range []uint64{0x98f2bc8e61, 0x79b76d99e2, 0xf33e5fb3c4, 0xae2eabe2a8, 0x1e4f43e470} {
			if c0&(1<<i) != 0 {
				c ^= g
			}
		}
	}
	return c
}
