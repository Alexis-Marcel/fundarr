# Contributing

Most contributions are a new **source** (a platform Fundarr reads from) or a new **destination** (an app Fundarr sends to, listed under Apps). Each one is a Go package that registers itself; the engine, storage and UI need no change.

## A new source

Create `internal/source/<kind>/` and register a definition in `init()`:

```go
source.Register(source.Definition{
    Kind:     "kraken",
    Name:     "Kraken",
    Category: "exchange", // exchange, broker or wallet
    Fields: []source.Field{
        {Name: "api_key", Label: "API key"},
        {Name: "api_secret", Label: "Private key", Secret: true},
    },
    Setup: "How to create a read-only key, shown above the form.",
    New:   func(cfg source.Config) (source.Source, error) { … },
})
```

Then import the package for its side effect in `cmd/fundarr/main.go`.

A source implements:

- `Activities(ctx, f)`: emit every movement since `f.Since` (zero means the whole history) with `f.Emit`. Each `model.Activity` needs a **stable `NativeID`**, because the engine deduplicates on it. Re-fetching a few days of overlap is fine and expected.
- `Balances(ctx)`: what the platform says is held now.
- `State()`: optional cursors, given back next run in `cfg.State`.
- `Test(ctx)` (optional, recommended): check the credentials, and **refuse keys that can trade or withdraw** when the API tells you.

Rules that keep the data trustworthy:

- Amounts are `decimal.Decimal` parsed from the API's strings. Never go through `float64`.
- Times are UTC.
- Internal moves within the platform (spot ↔ funding ↔ earn) are not activities.
- A fee goes in `Fee` with its own asset. Rebates are a `Reward`.
- A test must show that replaying the fixtures with `model.Reconcile` matches the fixture balances. That is the contract every source is held to.
- Tests use `httptest` fixtures shaped exactly like the documented responses, and never call the real API.

## A new destination

Create `internal/dest/<kind>/`, register a `dest.Definition`, and implement:

- `Accounts(ctx)`: where activities can go (for the mapping screen).
- `Push(ctx, batch)`: write the activities and return one `Result` per activity: `Delivered`, `Duplicate`, `Skipped` (not representable, not retried) or `Failed` (retried). It must be safe to call again with activities that were already sent.

Translate from the model's vocabulary into the destination's. Look at `internal/dest/wealthfolio` for trades priced in stablecoins, fees in a third asset, and crypto-to-crypto trades.

## Checks

```sh
go vet ./... && go test -race ./...
cd web && npm run build
```
