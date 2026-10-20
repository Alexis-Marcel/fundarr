# Read API

Any program can read what Fundarr knows, for trackers Fundarr does not push to, scripts and automations.

Create a token in **Settings → Export and API**, then send it with each request:

```
Authorization: Bearer fdr_…
```

A token only reads `/api/v1`; it cannot change anything. Revoke it in Settings.

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

## `GET /api/v1/holdings`

What each source reported holding at its last sync.

## Portfolio (crypto)

Values are in the currency of Settings → Valuation (`currency` in every answer). Only crypto counts (stablecoins included); fiat and securities are left out. An asset with no known price is listed in `unpriced` and counted at 0.

**Invested** is the money that came in from outside the crypto portfolio: fiat spent buying crypto, minus fiat received selling it, plus crypto deposited from outside (and balance corrections that add coins) at its value that day, minus crypto sent outside (and corrections that remove coins) at its value that day. Moves between your own sources are not invested. Rewards are gains, not invested.

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
- `realized`: gains on what was sold or spent, with the same average cost, all time.
- `rewards`: value of rewards received, at their day's price, all time.
- `share`: part of `total.value`, 0 to 1. Assets are sorted by value, largest first.

### `GET /api/v1/portfolio/history?from=&to=&step=day|week|month`

One point per step (end of day UTC), `from` and `to` as `YYYY-MM-DD` (default: from the first activity to today), `step` default `day`.

```json
{
  "currency": "EUR",
  "points": [{ "date": "2024-03-01", "value": "1520.00", "invested": "1500.00" }]
}
```

`value`: what the crypto held that day was worth at that day's close. `invested`: cumulative, as defined above.

### `GET /api/v1/tax/fr?year=2025`

French tax on crypto disposals (article 150 VH bis du CGI, formulaire 2086), for one calendar year. Only disposals for fiat money (or goods and services) are taxable events; crypto-to-crypto exchanges, stablecoins included, are not. Each disposal's gain is:

```
gain = price − acquisition × price / portfolio
```

- `price`: what was received, in EUR, minus the fees of that disposal;
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
      "capitalShare": "636.65",
      "gain": "212.15"
    }
  ],
  "total": { "disposals": "850.00", "gain": "212.15", "exempt": false, "tax": "63.65", "rate": "0.30" },
  "notes": ["Disposals totalling 305 € or less in the year are exempt."]
}
```

- Lines follow the 2086 form: `price` (cases 213/214/217), `portfolio` (212), `acquisition` (220…223), `capitalShare` = acquisition × price / portfolio.
- `exempt`: the year's disposals total 305 € or less.
- `tax`: `gain` (if positive) × `rate` (flat tax), as an estimate.
