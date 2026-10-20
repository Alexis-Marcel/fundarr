# Read API

Any program can read what Fundarr knows, for trackers Fundarr does not push to, scripts and automations.

In Fundarr, **Apps → Add app → Other app** (or Cairn) makes a token; send it with each request:

```
Authorization: Bearer fdr_…
```

A token only reads `/api/v1`; it cannot change anything. Remove the app to revoke it.

The routes are also described in [`openapi.yaml`](openapi.yaml), served at `GET /api/v1/openapi.yaml` (no token needed). To try them without accounts, run Fundarr with `FUNDARR_DEMO=1`: see [Build an app](build-an-app.md).

## Stability

`/api/v1` only grows: new routes and new fields may appear, and clients should ignore fields they don't know. Nothing is renamed or removed within v1; a breaking change would come as `/api/v2`, with v1 kept alongside for a while. The Go packages under `pkg/` follow [semver](https://semver.org) with the releases: before 1.0, a minor version (0.2) may break them, and its release notes say how.

## `GET /api/v1/sources`

```json
[{ "id": "9b687b4899f5", "name": "Binance", "kind": "binance" }]
```

## `GET /api/v1/activities`

Every activity of every source, oldest first.

| Parameter | |
|---|---|
| `source` | one source id |
| `since`, `until` | RFC 3339 times (`until` excluded) |
| `limit` | 1 to 5000, default 500 |
| `after` | the `next` value of the previous page |

```json
{
  "activities": [
    {
      "id": "6f1c…",
      "source": { "id": "9b687b4899f5", "name": "Binance", "kind": "binance" },
      "account": "",
      "time": "2024-03-01T10:00:00Z",
      "type": "trade",
      "side": "buy",
      "base": { "asset": { "kind": "crypto", "symbol": "BTC" }, "qty": "0.01" },
      "quote": { "asset": { "kind": "fiat", "symbol": "EUR" }, "qty": "550" },
      "fee": { "asset": { "kind": "crypto", "symbol": "BNB" }, "qty": "0.001" },
      "cost": "550.00",
      "currency": "EUR"
    }
  ],
  "next": "1709287200000_6f1c…"
}
```

- `type`: `trade`, `deposit`, `withdrawal`, `reward`, `fee`, `dividend`, `interest`, `tax`, `adjustment`.
- `cost`: what an incoming asset cost, in `currency` (Settings → Valuation).
- `transferWith`: the other side of a move between two of your sources.
- `correction: true`: added by Fundarr's balance check, not by the platform.

## `GET /api/v1/activities.csv?format=fundarr|koinly`

The same list as a CSV file: every field (`fundarr`), or Koinly's universal format (`koinly`). `source` filters one source.

## `GET /api/v1/changes`

For an app that keeps its own copy of the history: the first call (no `after`) returns every activity; each next call, with the `cursor` of the previous answer, returns what was added, rewritten or removed since, in order.

| Parameter | |
|---|---|
| `after` | the `cursor` of the previous answer; leave it out for a first copy |
| `limit` | 1 to 5000, default 1000 |

```json
{
  "changes": [
    { "op": "upsert", "id": "6f1c…", "activity": { "id": "6f1c…", "source": { "id": "9b68…", "name": "Binance", "kind": "binance" }, "type": "deposit", "transferWith": "a2e0…", … } },
    { "op": "remove", "id": "d93b…" }
  ],
  "cursor": "4e1a9c02.1873",
  "more": false
}
```

- `upsert`: add the activity, or replace the one with this `id`. It has the shape of `/api/v1/activities`, without `cost` and `currency` (they move with prices and Settings → Valuation, not with the history: compute them, or read `/api/v1/activities`).
- `remove`: delete the activity with this `id`; one you do not have is a no-op.
- `cursor`: opaque; store it with your copy and pass it as `after` next time, even when `changes` is empty. While `more` is true, call again right away.
- Each activity appears once, in its latest state. Everything that rewrites history shows: new syncs, balance corrections made, recomputed or reclassified, a statement replacing a period, transfers paired or unpaired, a source renamed (`source.name`) or removed (all its activities).
- Corrections (`correction: true`) appear as soon as Fundarr makes them, like in `/api/v1/activities`; they may be removed by a later sync when the platform's history catches up.
- **410**: the cursor is too old (removals are kept 180 days) or from another Fundarr (a new database, a restarted demo): drop your copy and start again without `after`.

### Notify

So the app does not have to poll: in **Apps**, the app → **Notify**, give an address. After changes Fundarr POSTs to it (bursts are told once, a few retries if it fails, syncs never wait):

```json
{ "event": "changes", "cursor": "4e1a9c02.1873" }
```

No data: call `/api/v1/changes` from your own cursor. The secret shown once when the address is saved signs each call:

```
Fundarr-Signature: t=1729425600,v1=5d2c…
```

`v1` is the hex HMAC-SHA256 of `<t>.<body>` with the secret; check it, and that `t` is recent. Saving the address again makes a new secret.

## `GET /api/v1/holdings`

What each source reported holding at its last sync, account by account. An `account` of `"*"` is one figure the platform gave for all the source's accounts together; `unread: true` marks a balance the platform did not say, its `qty` being what the history adds up to.

## Portfolio

Values are in the currency of Settings → Valuation (`currency` in every answer). Crypto (stablecoins included) and securities (stocks, ETFs) count; fiat is left out. A security with no market price is valued at its last trade in the history. An asset with no known price is listed in `unpriced` and counted at 0 (left out of `assets`).

**Invested** is the money that came in from outside the portfolio: fiat spent buying crypto or securities, minus fiat received selling them, plus what was deposited from outside (and balance corrections that add coins) at its value that day, minus what was sent outside (and corrections that remove coins) at its value that day. Moves between your own sources are not invested. Rewards are gains, not invested.

### `GET /api/v1/portfolio`

```json
{
  "currency": "EUR",
  "at": "2026-10-20T12:00:00Z",
  "total": {
    "value": "12450.20",
    "invested": "9800.00",
    "cost": "9100.00",
    "unrealized": "3350.20",
    "realized": "420.00",
    "rewards": "130.50"
  },
  "assets": [
    {
      "asset": { "kind": "crypto", "symbol": "BTC" },
      "qty": "0.12",
      "price": "61000.00",
      "value": "7320.00",
      "cost": "5100.00",
      "unrealized": "2220.00",
      "share": "0.588",
      "sources": [{ "id": "9b68…", "name": "Binance", "qty": "0.02" }, { "id": "0130…", "name": "Ledger", "qty": "0.10" }]
    }
  ],
  "sources": [{ "id": "9b68…", "name": "Binance", "kind": "binance", "value": "2100.00" }],
  "unpriced": [{ "kind": "crypto", "symbol": "EURPS" }]
}
```

- `cost`: average cost of what is held (Fundarr's cost basis); `unrealized` = `value` − `cost`.
- `realized`: gains on what was sold for money or stablecoins, spent or sent outside, with the same average cost, all time, fees deducted. A swap for another coin carries its cost over to it (no gain until that coin is sold).
- `rewards`: value of rewards received, at their day's price, all time.
- `share`: part of `total.value`, 0 to 1. Assets are sorted by value, largest first.
- `value` − `invested` = `unrealized` + `realized` + `rewards` (up to stablecoins' exchange rate moves).

### `GET /api/v1/portfolio/history?from=&to=&step=day|week|month`

One point per step (end of day UTC), `from` and `to` as `YYYY-MM-DD` (default: from the first activity to today), `step` default `day`. A `week` ends on Sunday, a `month` on its last day; the last point is `to`. At most 3700 points (otherwise 422: use a longer step).

```json
{
  "currency": "EUR",
  "points": [{ "date": "2024-03-01", "value": "1520.00", "invested": "1500.00" }],
  "unpriced": []
}
```

`value`: what was held that day was worth at that day's close. `invested`: cumulative, as defined above.

### `GET /api/v1/tax/fr?year=2025&rewards=income`

French tax on crypto disposals (article 150 VH bis du CGI, formulaire 2086), for one calendar year (default: last year), always in EUR. Only disposals for fiat money (or goods and services) are taxable events; crypto-to-crypto exchanges, stablecoins included, are not. Each disposal's gain is:

```
gain = price − fees − acquisition × price / portfolio
```

- `price`: what was received, in EUR; `fees`: the fees of that disposal (they reduce the gain, not the ratio);
- `portfolio`: value of all the crypto held across every source just before the disposal (day's prices);
- `acquisition`: total price paid for all crypto ever acquired with fiat money (fees included), minus the share of it already used by earlier disposals (the "fraction de capital initial" of each previous line). Crypto received for free (rewards, airdrops) or deposited from outside adds its value at reception only if `rewards=income` is passed (they were taxed as income); otherwise 0.

```json
{
  "year": 2025,
  "currency": "EUR",
  "disposals": [
    {
      "time": "2025-04-02T09:12:00Z",
      "source": { "id": "9b68…", "name": "Binance" },
      "asset": { "kind": "crypto", "symbol": "BTC" },
      "qty": "0.01",
      "price": "850.00",
      "fees": "1.20",
      "portfolio": "12000.00",
      "acquisition": "9000.00",
      "capitalShare": "637.50",
      "gain": "211.30"
    }
  ],
  "total": { "disposals": "850.00", "gain": "211.30", "exempt": false, "tax": "66.35", "rate": "0.314" },
  "notes": ["Disposals totalling 305 € or less in the year are exempt."]
}
```

- Lines follow the 2086 form: `portfolio` (212), `price` (213, 217), `fees` (214), `acquisition` (223), `capitalShare` = acquisition × price / portfolio (the next lines' 221), `gain` (224). Amounts are rounded to the cent; the form takes whole euros.
- Earlier disposals, of any year and even exempt ones, have already reduced `acquisition`.
- `exempt`: the year's disposals total 305 € or less.
- `gain`: the year's net gain or loss (lines 224 added up).
- `tax`: `gain` (if positive, not exempt) × `rate` (flat tax: 12.8 % income tax and social charges), as an estimate: 0.30 for 2019–2024, 0.314 from 2025 income (CSG raised to 10.6 % by the LFSS 2026).
- `notes`: what the figures assume (daily closing prices instead of the exact time, unpriced assets counted at 0…).
