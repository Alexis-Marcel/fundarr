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

const receive = "Receive → copy the address.";

export const walletApps: WalletApp[] = [
  {
    id: "ledger",
    name: "Ledger",
    kind: "hardware",
    intro: "Add each account from Ledger Live.",
    steps: [
      {
        kinds: ["bitcoin", "litecoin", "dogecoin", "bitcoincash"],
        title: "Bitcoin and similar",
        how: "Account → ⋯ → Edit account → Advanced → copy the key.",
      },
      { kinds: ["evm"], title: "Ethereum", how: receive + " One address covers every network." },
      { kinds: ["solana", "xrp", "cardano", "tron"], title: "Other coins", how: receive },
    ],
  },
  {
    id: "trezor",
    name: "Trezor",
    kind: "hardware",
    intro: "Add each account from Trezor Suite.",
    steps: [
      {
        kinds: ["bitcoin", "litecoin", "dogecoin", "bitcoincash"],
        title: "Bitcoin and similar",
        how: "Account → Details → Show public key → copy it.",
      },
      { kinds: ["evm"], title: "Ethereum", how: receive + " One address covers every network." },
      { kinds: ["solana", "xrp", "cardano"], title: "Other coins", how: receive },
    ],
  },
  {
    id: "metamask",
    name: "MetaMask",
    kind: "app",
    intro: "Add each MetaMask account.",
    steps: [{ kinds: ["evm", "solana", "bitcoin"], title: "Any account", how: "Click the account name to copy its address." }],
  },
  {
    id: "phantom",
    name: "Phantom",
    kind: "app",
    intro: "Add each address Phantom shows.",
    steps: [{ kinds: ["solana", "evm", "bitcoin"], title: "Solana, Ethereum, Bitcoin", how: receive }],
  },
  {
    id: "trust",
    name: "Trust Wallet",
    kind: "app",
    intro: "Add each coin you hold.",
    steps: [
      { kinds: ["evm", "bitcoin", "solana", "xrp", "tron", "cardano"], title: "Any coin", how: "Tap the coin → " + receive },
    ],
  },
  {
    id: "exodus",
    name: "Exodus",
    kind: "app",
    intro: "Add each coin you hold.",
    steps: [
      {
        kinds: ["bitcoin", "litecoin", "dogecoin", "bitcoincash"],
        title: "Bitcoin and similar",
        how: "Asset → ⋯ → View public key → copy it.",
      },
      { kinds: ["evm", "solana", "xrp", "tron", "cardano"], title: "Other coins", how: "Asset → " + receive },
    ],
  },
];

export const otherApp: WalletApp = {
  id: "other",
  name: "Wallet",
  kind: "app",
  intro: "Paste each account's key or address.",
  steps: [
    { kinds: ["bitcoin", "litecoin", "dogecoin", "bitcoincash"], title: "Bitcoin and similar", how: "The public key (xpub…) or the address." },
    { kinds: ["evm", "solana", "xrp", "tron", "cardano"], title: "Other coins", how: "The receive address." },
  ],
};

export function walletApp(id?: string): WalletApp {
  return walletApps.find((a) => a.id === id) ?? otherApp;
}
