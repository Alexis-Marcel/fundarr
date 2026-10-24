# Sources

Every source uses read-only access. When a connector can tell that a key also allows trading or withdrawals, it refuses the key. Each source's form in Fundarr shows how to create its key.

## Binance

History since 2017, with a read-only API key: spot trades, converts, Auto-Invest, card and bank purchases, Simple Earn rewards, ETH and SOL staking, distributions, dust conversions, Binance Pay (18 months), P2P (6 months).

Binance's API returns spot trades from about the last two years only, and none on delisted pairs, and Binance has no API for its transaction history export. Fundarr corrects the balances for what is missing; importing the export (**Import statement** on the source) replaces the corrections with the real trades. Binance limits each export to a few months: export each period from your first deposit to today and import the files one by one, in any order; each becomes the history for the dates it covers, and re-importing a period replaces it without duplicates.

## OKX

History since February 2021, with a read-only v5 key. Older trades come from OKX's quarterly archives, ready about 2 hours after the first sync. Bank (SEPA) deposits and withdrawals included. Regional domains (EEA, US) are detected.

## Kraken

The whole ledger, with a read-only key (Query Funds, Query Ledger Entries): trades, Buy Crypto, deposits, withdrawals, staking and Earn rewards.

## Coinbase

The whole history, with a CDP key that has the View permission only: buys, sells, converts, Advanced Trade fills, sends with network fee and hash, fiat, staking and other rewards.

## Bitpanda

The whole history, with a read-only key (Balances, Transaction) on the new Public API: crypto, stocks and ETFs (by ISIN), metals, savings plans, staking and BEST rewards, dividends.

## Interactive Brokers (beta)

**Beta:** built from IBKR's documentation and not yet checked against a real account. If a sync looks wrong, please open an issue with the source's log (it holds no secrets).

Through IBKR's official, read-only Flex Web Service. In the Client Portal, open **Performance & Reports → Flex Queries**, create an **Activity Flex Query** in **XML** with the sections Trades (Executions), Cash Transactions, Open Positions, Cash Report, Corporate Actions and Transfers, and note its Query ID. Under **Flex Web Service Configuration**, enable the service and generate a token. Fundarr needs the token and the Query ID; the token can only read reports.

- Trades by ISIN with their commission and transaction taxes, currency conversions, deposits and withdrawals, dividends and withholding tax, interest, fees, position transfers in and out (ACATS, internal), splits and other corporate actions. Balances are the open positions and the cash per currency from the Cash Report.
- IBKR returns at most 365 days per report, as of yesterday's close. The first sync reads back a year at a time until it finds two empty years; set **Account opened on** to read exactly from that date. Later syncs read from a week before the last one.
- Options, futures, CFDs and other derivatives are skipped (and logged), so their cash shows as a balance correction. When a query covers several accounts, only the first is read: make one query per account.
- Times in Flex reports are New York time and are converted to UTC. A reverse split or a merger into a new ISIN is booked as the old shares sold for the cash paid (often nothing) and the new ones bought for nothing.

## Trade Republic

Compte-titres, PEA and crypto, each account apart (the PEA can go to its own app account). Two ways, picked in the form:

**CSV export.** In the app: Profile → Statements → Transaction export → the whole period. Import the file on the source. A newer export later replaces the period it covers; nothing is duplicated.

**Connect (beta).** Fundarr signs in to Trade Republic's web API like its website does: phone number and PIN (stored encrypted), or a QR code. Tap **Sync now**: the Trade Republic app asks you to approve, then Fundarr downloads the same export and reads the balances. The session lasts 24 hours; syncs in that time need nothing from you, then the source waits for the next approval and is not retried. **Copy sync link** gives an address (`/sources/{id}/sync`) to keep on your phone's home screen: one tap syncs. Trade Republic does not offer this API and its terms likely forbid using it: use it knowingly. A refused PIN is never tried again until you change it.

Fundarr reads the web app's current version from app.traderepublic.com before signing in; to send another one, set it under Advanced (or `FUNDARR_TR_APP_VERSION`). Every sign-in and sync step is in the source's log, without secrets.

Limits:

- Dates are booking dates, not execution times; crypto has no transaction hash.
- In CSV mode, balances are what the history adds up to: there is nothing to check them against.
- A split adds shares at no cost; a reverse split, a spin-off or a merger whose other side is missing are approximations (rewards, or a sale for nothing). A dividend Trade Republic books again cancels its first booking when both are in the export; otherwise the reversal is a withdrawal.
- Stocks and ETFs are priced from justETF by ISIN (Yahoo Finance as a fallback); a security neither knows is valued at its last trade.

## Wallet

A hardware wallet (Ledger, Trezor…) or a wallet app (MetaMask, Phantom, Trust Wallet, Exodus…), every account in one source. Pick the app for a step-by-step guide, then paste each key or address: Fundarr recognizes its blockchain and shows what the account holds before you save. Fundarr never needs a recovery phrase.

| Chain | Paste | Read from |
|---|---|---|
| Bitcoin, Litecoin, Dogecoin, Bitcoin Cash | The extended public key (xpub, ypub, zpub…), or a single address | Esplora or Blockbook servers, with failover; your own under Advanced |
| Ethereum and EVM | The 0x… address | Ethereum, Optimism, Avalanche; Arbitrum, Base, Polygon, Gnosis with a free [Blockscout](https://dev.blockscout.com) key; BNB Chain with an Etherscan key whose plan covers it |
| Solana | The address | SOL and verified SPL tokens, public RPC or your own |
| Tron | The address | TRX and TRC-20 tokens (TronGrid) |
| Cardano | The address | ADA and staking rewards (Koios) |
| XRP | The address | XRP and tokens from known issuers |

With an extended public key, every address type in use is read. A send between two accounts of the same wallet counts as its fee only.

## Adding a source

A source is one Go package that registers itself: see [CONTRIBUTING.md](../CONTRIBUTING.md).
