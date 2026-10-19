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
