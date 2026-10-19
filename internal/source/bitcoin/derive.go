package bitcoin

import (
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
)

// deriveAddress returns the address at chain/index (0 = receive, 1 = change)
// under the account key, in the given format.
func deriveAddress(acct *hdkeychain.ExtendedKey, t scriptType, chain, index uint32) (string, error) {
	c, err := acct.Derive(chain)
	if err != nil {
		return "", err
	}
	k, err := c.Derive(index)
	if err != nil {
		return "", err
	}
	pub, err := k.ECPubKey()
	if err != nil {
		return "", err
	}
	params := &chaincfg.MainNetParams
	h := btcutil.Hash160(pub.SerializeCompressed())
	var a btcutil.Address
	switch t {
	case p2pkh:
		a, err = btcutil.NewAddressPubKeyHash(h, params)
	case p2wpkh:
		a, err = btcutil.NewAddressWitnessPubKeyHash(h, params)
	case p2shP2wpkh:
		// The redeem script is the witness program: OP_0 <20-byte hash>.
		a, err = btcutil.NewAddressScriptHash(append([]byte{0x00, 0x14}, h...), params)
	case p2tr:
		// BIP86: key-path only, tweaked with no script tree.
		a, err = btcutil.NewAddressTaproot(schnorr.SerializePubKey(txscript.ComputeTaprootKeyNoScript(pub)), params)
	}
	if err != nil {
		return "", err
	}
	return a.EncodeAddress(), nil
}
