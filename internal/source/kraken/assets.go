package kraken

import (
	"context"
	"strings"
)

// Kraken names assets with its own codes. Old ones carry an X (crypto) or Z
// (fiat) prefix: XXBT, XETH, ZEUR. The prefix cannot be stripped blindly
// (ZETA, ZEUS, XION, XTZ, ZRX are real tickers), so the altname from
// /0/public/Assets is used, with the legacy codes below when that table
// cannot be read. Kraken's own tickers then become the usual ones (XBT is
// BTC, XDG is DOGE).
var legacy = map[string]string{
	"XXBT": "XBT", "XETH": "ETH", "XLTC": "LTC", "XXRP": "XRP", "XXLM": "XLM", "XXMR": "XMR",
	"XXDG": "XDG", "XETC": "ETC", "XMLN": "MLN", "XREP": "REP", "XZEC": "ZEC",
	"ZUSD": "USD", "ZEUR": "EUR", "ZGBP": "GBP", "ZCAD": "CAD", "ZJPY": "JPY", "ZAUD": "AUD",
	"ZARS": "ARS", "ZCLP": "CLP", "ZCOP": "COP", "ZDKK": "DKK", "ZGEL": "GEL", "ZGHS": "GHS",
	"ZLKR": "LKR", "ZMXN": "MXN", "ZPLN": "PLN", "ZSEK": "SEK", "ZUGX": "UGX", "ZVND": "VND", "ZXOF": "XOF",
}

var renamed = map[string]string{"XBT": "BTC", "XDG": "DOGE"}

// Suffixes of the same coin held elsewhere in the account: staked (.S),
// opt-in rewards (.M), bonded (.B), Earn flexible (.F), parachain (.P),
// funds on hold (.HOLD).
var variants = map[string]bool{"S": true, "M": true, "B": true, "F": true, "P": true, "HOLD": true}

// Staked variants whose name is not just the coin plus a suffix.
var stakedAlias = map[string]string{"ETH2": "ETH", "FLOWH": "FLOW"}

// feeCredits are Kraken fee credits (KFEE): not an asset, never counted.
const feeCredits = "KFEE"

// symbol turns a Kraken asset code into the coin it stands for ("" for fee
// credits). names maps codes to altnames (may be nil).
func symbol(code string, names map[string]string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" || code == feeCredits || code == "FEE" {
		return ""
	}
	base, staked := code, false
	if i := strings.LastIndexByte(code, '.'); i > 0 && variants[code[i+1:]] {
		base, staked = code[:i], true
	}
	if a, ok := stakedAlias[base]; ok {
		base = a
	} else if alt, ok := names[base]; ok && alt != "" {
		base = strings.ToUpper(alt)
	} else if alt, ok := legacy[base]; ok {
		base = alt
	}
	if staked {
		// Bonded staking with a lock period: DOT28.S, ATOM21.S, SOL03.S.
		if t := strings.TrimRight(base, "0123456789"); len(t) >= 2 && t != base {
			base = t
		}
		if a, ok := stakedAlias[base]; ok {
			base = a
		}
	}
	if r, ok := renamed[base]; ok {
		base = r
	}
	return base
}

// assetNames reads /0/public/Assets once per run; without it the legacy
// table is used.
func (s *Source) assetNames(ctx context.Context) map[string]string {
	s.namesOnce.Do(func() {
		var res map[string]struct {
			Altname string `json:"altname"`
		}
		if s.namesErr = s.c.public(ctx, "/0/public/Assets", &res); s.namesErr != nil {
			return
		}
		s.names = map[string]string{}
		for code, a := range res {
			s.names[strings.ToUpper(code)] = a.Altname
		}
	})
	return s.names
}
