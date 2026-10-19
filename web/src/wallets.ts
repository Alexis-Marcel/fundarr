// Wallet apps the Wallet source has a guide for: where each one shows the
// keys and addresses Fundarr reads. Chains are listed most common first.

export interface Step {
  /** Wallet chain kinds this step is about (icons). */
  kinds: string[];
  title: string;
  how: string;
}

export interface WalletApp {
  id: string;
  name: string;
  kind: "hardware" | "app";
  intro: string;
  steps: Step[];
}

const receive = (app: string) => `In ${app}, open the account, choose Receive and copy the address.`;

export const walletApps: WalletApp[] = [
  {
    id: "ledger",
    name: "Ledger",
    kind: "hardware",
    intro: "Add every account Ledger Live lists. Each blockchain, and each account of it, has its own key.",
    steps: [
      {
        kinds: ["bitcoin", "litecoin", "dogecoin", "bitcoincash"],
        title: "Bitcoin, Litecoin, Dogecoin, Bitcoin Cash",
        how:
          "In Ledger Live, open the account, then ⋯ (top right), Edit account, Advanced, and copy the key (xpub…). " +
          "A Bitcoin account may be Native SegWit, Taproot or Legacy: add each one that ever held coins.",
      },
      {
        kinds: ["evm"],
        title: "Ethereum and its networks",
        how:
          receive("Ledger Live") +
          " One 0x… address covers Ethereum, Polygon, Arbitrum, Base and the others: accounts that show the same address only need adding once.",
      },
      { kinds: ["solana", "xrp", "cardano", "tron"], title: "Solana, XRP, Cardano, Tron", how: receive("Ledger Live") },
    ],
  },
  {
    id: "trezor",
    name: "Trezor",
    kind: "hardware",
    intro: "Add every account Trezor Suite lists. Each blockchain, and each account of it, has its own key.",
    steps: [
      {
        kinds: ["bitcoin", "litecoin", "dogecoin", "bitcoincash"],
        title: "Bitcoin, Litecoin, Dogecoin, Bitcoin Cash",
        how: "In Trezor Suite, open the account, then Details, Show public key, confirm on the Trezor, and copy the key (xpub…, zpub…).",
      },
      { kinds: ["evm"], title: "Ethereum and its networks", how: receive("Trezor Suite") + " One 0x… address covers every Ethereum network." },
      { kinds: ["solana", "xrp", "cardano"], title: "Solana, XRP, Cardano", how: receive("Trezor Suite") },
    ],
  },
  {
    id: "metamask",
    name: "MetaMask",
    kind: "app",
    intro: "One address per MetaMask account, read on every network at once. For Arbitrum, Base, Polygon and Gnosis, add a free Blockscout key under Advanced.",
    steps: [
      {
        kinds: ["evm"],
        title: "Ethereum accounts",
        how: "Click the account name at the top of MetaMask to copy its 0x… address. Add each account you use.",
      },
      {
        kinds: ["solana", "bitcoin"],
        title: "Solana and Bitcoin accounts",
        how: "If you created some in MetaMask, select the account and copy its address the same way.",
      },
    ],
  },
  {
    id: "phantom",
    name: "Phantom",
    kind: "app",
    intro: "Phantom gives each account one address per blockchain.",
    steps: [
      {
        kinds: ["solana", "evm", "bitcoin"],
        title: "Solana, Ethereum, Bitcoin",
        how: "In Phantom, choose Receive and copy each address: Solana, then Ethereum (0x…), then Bitcoin. The Bitcoin one is a single address: paste it as it is.",
      },
    ],
  },
  {
    id: "trust",
    name: "Trust Wallet",
    kind: "app",
    intro: "Trust Wallet shows one address per blockchain.",
    steps: [
      {
        kinds: ["evm"],
        title: "Ethereum, BNB Chain and other networks",
        how: "Tap Ethereum, then Receive, and copy the 0x… address: it is the same on every EVM network.",
      },
      {
        kinds: ["bitcoin", "solana", "xrp", "tron", "cardano", "litecoin", "dogecoin"],
        title: "Every other crypto",
        how: "Tap the crypto, then Receive, and copy its address.",
      },
    ],
  },
  {
    id: "exodus",
    name: "Exodus",
    kind: "app",
    intro: "Exodus changes Bitcoin-like addresses as you use them, so those need the account key.",
    steps: [
      {
        kinds: ["bitcoin", "litecoin", "dogecoin", "bitcoincash"],
        title: "Bitcoin, Litecoin, Dogecoin, Bitcoin Cash",
        how: "Open the asset, then ⋯ or the wrench, View public key, and copy the key (xpub…).",
      },
      {
        kinds: ["evm", "solana", "xrp", "tron", "cardano"],
        title: "Ethereum, Solana, XRP, Tron, Cardano",
        how: "Open the asset, choose Receive and copy the address.",
      },
    ],
  },
];

export const otherApp: WalletApp = {
  id: "other",
  name: "Wallet",
  kind: "app",
  intro: "Paste each account's key or address; Fundarr recognizes the blockchain.",
  steps: [
    {
      kinds: ["bitcoin", "litecoin", "dogecoin", "bitcoincash"],
      title: "Bitcoin, Litecoin, Dogecoin, Bitcoin Cash",
      how: "The account's extended public key (xpub, ypub, zpub, Ltub, dgub), usually in the account's settings or details. A wallet that only ever uses one address: that address.",
    },
    {
      kinds: ["evm"],
      title: "Ethereum and EVM networks",
      how: "The 0x… address. Fundarr reads Ethereum, Optimism and Avalanche from it, and Arbitrum, Base, Polygon and Gnosis too with a free Blockscout key (Advanced).",
    },
    { kinds: ["solana", "xrp", "tron"], title: "Solana, XRP, Tron", how: "The account's receive address." },
    { kinds: ["cardano"], title: "Cardano", how: "A receive address (addr1…) or the stake address (stake1…); staking rewards are included." },
  ],
};

export function walletApp(id?: string): WalletApp {
  return walletApps.find((a) => a.id === id) ?? otherApp;
}
