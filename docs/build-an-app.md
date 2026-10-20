# Build an app on Fundarr

Fundarr already did the hard part: every exchange and wallet read, the history checked against the balances, transfers paired, costs and prices worked out. Your app reads the result from the [read API](api.md) and only has to show it.

## 1. Run the demo

```sh
docker run --rm -e FUNDARR_DEMO=1 -p 9494:9494 ghcr.io/alexis-marcel/fundarr
```

`FUNDARR_DEMO=1` serves a made-up crypto portfolio: Binance, Kraken and a Ledger from 2023 to today, with monthly buys, staking rewards, moves to the Ledger, a swap to USDC and a few sales. The engine computes everything from it as it would from real accounts, with fixed made-up prices (no network). It lives in memory: nothing is read from or written to `/data`, nothing syncs, and any token is accepted, `fdr_demo` for instance.

Open http://localhost:9494 to see the same data in Fundarr.

## 2. Make a request

```sh
curl -H "Authorization: Bearer fdr_demo" http://localhost:9494/api/v1/portfolio
```

The main routes:

| Route | |
|---|---|
| `GET /api/v1/portfolio` | Value, money invested, gains, each asset and where it is held |
| `GET /api/v1/portfolio/history?step=month` | Value and invested over time, for a chart |
| `GET /api/v1/tax/fr?year=2025` | The French 2086 form, line by line |
| `GET /api/v1/activities` | Every activity, paged |

The whole API is described in [`openapi.yaml`](openapi.yaml) (also served at `/api/v1/openapi.yaml`), to generate a client; [api.md](api.md) explains what the figures mean.

## 3. Keep the token on a server

A token reads your whole history. Never put it in a web page or a mobile app: anyone could read it there. Have your app's server call Fundarr with the token and pass the answers on to the browser, as Cairn does in a few lines of Go (`GET /api/*` → Fundarr's `/api/v1/*`).

## 4. Connect it to a real Fundarr

In Fundarr, **Apps → Add app → Other app**: name it, then copy the API URL and the token, shown once. Remove the app there to revoke its token.

When your app runs in Docker on the same server, reach Fundarr by its container name (`http://fundarr:9494`) on a shared network rather than through its public address: a reverse proxy that only lets your home network or VPN in sees the app as a stranger and blocks it. To find Fundarr without asking, an app can try those addresses: `GET /api/v1/openapi.yaml` needs no token and answers Fundarr's API description.

## Keep your own copy

An app with its own database stays in sync with [`/api/v1/changes`](api.md#get-apiv1changes):

1. Call `GET /api/v1/changes` without `after`: every activity.
2. Apply each change in order: `upsert` adds or replaces the activity with that `id`, `remove` deletes it. Store the `cursor` with your data, in the same transaction.
3. While `more` is true, call again with `after=<cursor>`. Later, call again from the stored cursor: only what changed comes back.
4. On **410**, the cursor is too old: empty your copy and start at 1.

To be told instead of polling, give the app a notify address in **Apps → Notify**; Fundarr POSTs `{"event":"changes","cursor":…}` there, signed with a secret shown once.

Costs are not in the feed: they depend on prices and on the currency, not only on the history. Read them from `/api/v1/activities`, or compute them as below.

## Compute it yourself (Go)

Fundarr's own code is importable, so a Go app can work out costs, the portfolio and the 2086 form from its copy, with its own rules on top:

| Package | |
|---|---|
| `github.com/Alexis-Marcel/fundarr/pkg/api` | The API's JSON types (`Activity`, `Changes`), `Copy.Apply` for the feed, `Ledger` for the rest |
| `github.com/Alexis-Marcel/fundarr/pkg/model` | Activities; `Books` and `Costs` (average cost per source, transfers at cost), `PairTransfers` |
| `github.com/Alexis-Marcel/fundarr/pkg/portfolio` | `Now`, `History`, `TaxFR` (2086), given a price function |
| `github.com/Alexis-Marcel/fundarr/pkg/price` | Daily prices from Binance, OKX and the ECB, no key; `UnitPrice` plugs it into `portfolio` |

```go
copy := api.Copy{}
copy.Apply(page) // each page of /api/v1/changes
acts, transfers := copy.Ledger()
prices := price.New(nil, http.DefaultClient).UnitPrice(ctx, "EUR")
tax := portfolio.TaxFR(acts, transfers, prices, 2025, false)
```

A full example is in [`pkg/portfolio/example_test.go`](../pkg/portfolio/example_test.go). These packages follow Fundarr's versions; anything under `internal/` is not for apps.

## A full example

[Cairn](https://github.com/Alexis-Marcel/cairn) is a small app built this way: a portfolio dashboard and the 2086 form, a Go server holding the token and a React UI.
