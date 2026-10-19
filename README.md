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
- **Destinations** receive new activities after every sync. Each delivery is recorded, so nothing is sent twice and failures are retried.
- **One small container**: a Go binary with the web UI built in, plus a SQLite file. It runs on a Raspberry Pi.

## Connectors

| Source | History | Notes |
|---|---|---|
| Binance | Since 2017 | Read-only API key. Spot trades are found per pair, starting from every asset the account ever touched. |
| OKX | Since Feb 2021 | Read-only v5 key. Older trading history comes from OKX's quarterly archives, ready about 2 hours after the first sync. |

| Destination | How |
|---|---|
| Wealthfolio | Through its MCP endpoint (`WF_MCP_ENABLED=true` and an access token). Stablecoins can count as USD or EUR cash, so crypto bought with them has a price. |

Next on the list: Trade Republic (via [pytr](https://github.com/pytr-org/pytr)), Bitcoin and EVM wallets from public addresses, Ghostfolio, and CSV. Adding one is a single Go package; see [CONTRIBUTING.md](CONTRIBUTING.md).

## Run it

```yaml
services:
  fundarr:
    image: ghcr.io/alexis-marcel/fundarr:latest
    restart: unless-stopped
    ports: ["9494:9494"]
    volumes: ["./fundarr:/data"]
    environment:
      FUNDARR_PASSWORD_HASH: "$$argon2id$$v=19$$m=65536,t=3,p=2$$…"   # docker run --rm -it ghcr.io/alexis-marcel/fundarr hash-password
```

On first start, Fundarr creates `/data/secret.key`, which encrypts the API keys stored in `/data/fundarr.db`. **Back up both together.** Without the key, the stored credentials cannot be read.

### Settings

| Variable | |
|---|---|
| `FUNDARR_PASSWORD_HASH` | Argon2id hash of the password (`fundarr hash-password`). |
| `FUNDARR_OIDC_ISSUER`, `FUNDARR_OIDC_CLIENT_ID`, `FUNDARR_OIDC_CLIENT_SECRET` | Single sign-on with any OpenID Connect provider (Authentik, Authelia, Keycloak, PocketID…). Redirect URI: `<FUNDARR_URL>/auth/callback`. |
| `FUNDARR_OIDC_ALLOWED_SUBS` / `FUNDARR_OIDC_ALLOWED_EMAILS` | Required with OIDC: who may sign in (comma-separated). A verified email is required for email matching. |
| `FUNDARR_URL` | Public URL, e.g. `https://fundarr.example.com`. |
| `FUNDARR_AUTH=none` | No login, for when a reverse proxy in front already authenticates. |
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
FUNDARR_AUTH=none FUNDARR_ADDR=127.0.0.1:9494 go run ./cmd/fundarr
```

## License

[AGPL-3.0](LICENSE). If you run a modified Fundarr for others, share your changes.
