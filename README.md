# Fundarr

**Prowlarr for your money.** Fundarr connects to your exchanges, brokers and wallets with read-only access, keeps their whole history in one normalized format, checks it against the real balances, and delivers it to the tools you already use, such as [Wealthfolio](https://wealthfolio.app).

Paid portfolio apps sell this sync. Fundarr is free, self-hosted, and your keys never leave your server.

```
 Sources                      Fundarr                         Destinations
 ───────                      ───────                         ────────────
 Binance  ─┐          normalize · dedupe · reconcile    ┌──▶  Wealthfolio
 OKX      ─┼──read-only──▶   activities + balances   ───┼──▶  (Ghostfolio, CSV…)
 …        ─┘                 SQLite, encrypted keys     └──▶
```

## What it does

- **Sources** fetch the full history: trades, converts, deposits and withdrawals (with tx hashes), staking and earn rewards, fees, dust conversions, card purchases. Every amount is an exact decimal.
- **Reconciliation.** Fundarr replays the history and compares it with what the platform reports. When the two match, the history is complete. When they don't, the gap is shown asset by asset, so you know before your portfolio tracker shows wrong numbers.
- **Corrections.** APIs forget: Binance's only returns about two years of spot trades. What the history does not explain becomes a balance correction, placed where it belongs (just before the coins are first spent) and valued at that day's price, so quantities and totals stay right. Corrections wait 48 hours before going out, in case the history catches up; you can say what one really was (a purchase, a reward, a transfer) and it becomes that activity.
- **Statements.** When an API leaves holes, import the platform's own export (Binance today): it becomes the history for its period, replacing the corrections with the real trades, keeping the API's richer details where both agree. Fundarr keeps everything it fetches, so an export is needed once, for the past.
- **Transfers.** A withdrawal on one source and the deposit it became on another (Binance to your Ledger) are paired, by transaction hash or by amount and time. Destinations move the coins between accounts at their purchase cost, instead of seeing a sale and a new purchase.
- **Prices.** Daily prices from Binance and OKX public market data, exchange rates from the European Central Bank (via Frankfurter): no account or key. They value corrections, rewards and airdrops, which then reach destinations with a cost.
- **Destinations** receive new activities after every sync. Each delivery is recorded, so nothing is sent twice and failures are retried.
- **One small container**: a Go binary with the web UI built in, plus a SQLite file. It runs on a Raspberry Pi.

## Connectors

| Source | History | Notes |
|---|---|---|
| Binance | Since 2017 | Read-only API key. Spot trades, converts, Auto-Invest, card and bank purchases, Simple Earn rewards, ETH/SOL staking, distributions, dust, Binance Pay (18 months), P2P (6 months). Binance's API returns spot trades from about the last two years only, and none on delisted pairs; Binance offers no API for its transaction history export. Fundarr corrects the balances, and importing the export once replaces the corrections with the real trades. |
| OKX | Since Feb 2021 | Read-only v5 key. Older trading history comes from OKX's quarterly archives, ready about 2 hours after the first sync. Bank (SEPA) deposits and withdrawals included. Regional domains (EEA, US) detected. |
| Wallet (Ledger, Trezor…) | Full | Every account of a self-custody wallet in one source, one key or address per line. Bitcoin: extended public key (xpub, ypub, zpub), every address type in use, any Esplora server (mempool.space, then blockstream.info, or your own). Ethereum & EVM: the 0x… address, read on every network (Ethereum, Arbitrum, Base, Optimism, Polygon, Gnosis, Avalanche, and BNB Chain with an Etherscan key whose plan covers it): ETH, tokens, gas. A send between two accounts of the wallet counts as its fee only. |

| Destination | How |
|---|---|
| Wealthfolio | Through its MCP endpoint (`WF_MCP_ENABLED=true` and an access token). Stablecoins can count as USD or EUR cash, so crypto bought with them has a price. |

Next on the list: Trade Republic (via [pytr](https://github.com/pytr-org/pytr)), Solana, Ghostfolio, CSV. Adding one is a single Go package; see [CONTRIBUTING.md](CONTRIBUTING.md).

## Run it

```sh
docker run -d --name fundarr -p 9494:9494 -v ./fundarr:/data ghcr.io/alexis-marcel/fundarr:latest
```

Open http://localhost:9494 and create your account, as in Sonarr or Prowlarr. For safety, the account can only be created from your local network or VPN.

Or with Compose:

```yaml
services:
  fundarr:
    image: ghcr.io/alexis-marcel/fundarr:latest
    restart: unless-stopped
    ports: ["9494:9494"]
    volumes: ["./fundarr:/data"]
```

On first start, Fundarr creates `/data/secret.key`, which encrypts the API keys stored in `/data/fundarr.db`. **Back up both together.** Without the key, the stored credentials cannot be read.

### Sign-in

| | |
|---|---|
| **Username and password** (default) | Created in the UI on first launch; change it in Settings. |
| **Single sign-on** | Any OpenID Connect provider (Authentik, Authelia, Keycloak, PocketID…): set `FUNDARR_OIDC_ISSUER`, `FUNDARR_OIDC_CLIENT_ID`, `FUNDARR_OIDC_CLIENT_SECRET`, `FUNDARR_URL`, and who may sign in with `FUNDARR_OIDC_ALLOWED_SUBS` or `FUNDARR_OIDC_ALLOWED_EMAILS` (comma-separated; emails must be verified). Redirect URI: `<FUNDARR_URL>/auth/callback`. |
| **Reverse proxy** | `FUNDARR_AUTH=proxy`: your proxy (Authelia, Authentik outpost…) authenticates and passes the user in `Remote-User` (or `FUNDARR_PROXY_HEADER`). Fundarr then must only be reachable through that proxy. |
| **None** | `FUNDARR_AUTH=none`, for a machine only you can reach. |

### Other settings

| Variable | |
|---|---|
| `FUNDARR_OIDC_AUTO_REDIRECT` | `false` shows a Sign in button instead of going to the identity provider right away (default: right away, so an open SSO session signs in without a click). |
| `FUNDARR_PASSWORD_HASH`, `FUNDARR_USERNAME` | Fix the credentials from the environment instead of the UI (`docker run --rm -it ghcr.io/alexis-marcel/fundarr hash-password`; in Compose, double every `$`). |
| `FUNDARR_SECRET_KEY` | Master key in base64 (`openssl rand -base64 32`), instead of the key file. |
| `FUNDARR_DATA`, `FUNDARR_ADDR` | Data directory (`/data`) and listen address (`:9494`). |

## Security

- Use **read-only** API keys. When a connector can tell that a key also allows trading or withdrawals, it refuses the key.
- Keys are encrypted at rest (AES-256-GCM) and never sent back to the browser.
- Restrict keys to your server's IP when the platform allows it.
- Put Fundarr behind your VPN or SSO; it does not need to be public.

## Develop

```sh
go test ./...                          # engine, connectors, API
cd web && npm install && npm run dev   # UI on :5173, API proxied to :9494
FUNDARR_DATA=./data FUNDARR_ADDR=127.0.0.1:9494 go run ./cmd/fundarr
```

## License

[AGPL-3.0](LICENSE). If you run a modified Fundarr for others, share your changes.
