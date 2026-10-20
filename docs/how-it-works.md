# How it works

```
 Sources                 Fundarr                          Apps
 Binance  ─┐                                        ┌──▶  Wealthfolio, Ghostfolio
 Kraken   ─┼─ read-only ─▶  one checked history  ───┼──▶  Cairn, your own app
 Ledger…  ─┘                                        └──▶  CSV (Koinly…)
```

**Sync.** Every 6 hours, or on demand, each source fetches its new activities: trades, converts, deposits and withdrawals (with tx hashes), staking and earn rewards, fees, dust conversions, card purchases. Every amount is an exact decimal. Fundarr keeps everything it fetches.

**Balance check.** Fundarr replays the history and compares it with the balances the platform reports. When the two match, the history is complete. When they don't, the gap is shown asset by asset, before your tracker shows wrong numbers.

**Corrections.** APIs forget: Binance's only returns about two years of spot trades. What the history does not explain becomes a balance correction, placed just before the coins are first spent and valued at that day's price, so quantities and totals stay right. Corrections wait 48 hours before going out, in case the history catches up. You can say what one really was (a purchase, a reward, a transfer) and it becomes that activity.

**Statements.** When an API leaves holes, import the platform's own export once (Binance today). It becomes the history for its period and replaces the corrections with the real trades.

**Transfers.** A withdrawal on one source and the deposit it became on another (Binance to your Ledger) are paired, by transaction hash or by amount and time. Apps then see coins moving at their purchase cost, not a sale and a new purchase.

**Prices.** Daily prices from Binance and OKX public market data, exchange rates from the European Central Bank (via Frankfurter): no account or key. They value corrections, rewards and airdrops, so they reach apps with a cost.

**Delivery.** Trackers such as Wealthfolio receive new activities after every sync. Each delivery is recorded, so nothing is sent twice and failures are retried. Other apps read Fundarr through the [read API](api.md) whenever they want.

**Storage.** One SQLite file. API keys are encrypted with AES-256-GCM under a master key kept next to it (`secret.key`), and never sent back to the browser.

## Apps Fundarr sends to

| App | Details |
|---|---|
| Wealthfolio | Through its MCP endpoint (`WF_MCP_ENABLED=true` and an access token). Stablecoins can count as USD or EUR cash, so crypto bought with them has a price. |
| Ghostfolio | Through its import API with your security token. Crypto by CoinGecko id or Yahoo symbol, securities by ISIN; transfers keep their cost. Cash moves are skipped (Ghostfolio keeps cash as a balance). |

You choose which account of the app each source goes to.
