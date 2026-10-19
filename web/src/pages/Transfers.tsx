import { useState } from "react";
import { api, type Side, type Transfers } from "../api";
import { assetLabel, dateTime, money, qty, usePoll } from "../lib";
import { CryptoIcon } from "../components/CryptoIcon";

const BY: Record<string, string> = {
  txid: "same transaction",
  amount: "same amount",
  user: "paired by you",
};

function where(s: Side) {
  return (
    <>
      <strong>{s.source || "?"}</strong> <span className="muted small">{dateTime(s.time)}</span>
    </>
  );
}

export function TransfersPage() {
  const { data, error, reload } = usePoll(() => api.transfers(), 60000);
  return (
    <section className="page">
      <header className="page-head">
        <div>
          <h1>Transfers</h1>
          <p className="lede">
            Coins you moved between your own accounts, like Binance to your Ledger. Fundarr pairs the withdrawal with the deposit, so destinations
            move them from one account to the other at their purchase cost instead of counting a sale and a new purchase.
          </p>
        </div>
      </header>
      {error && <p className="notice notice-error">{error}</p>}
      {data && <Body data={data} reload={reload} />}
    </section>
  );
}

function Body({ data, reload }: { data: Transfers; reload: () => void }) {
  const [busy, setBusy] = useState(false);
  const act = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    try {
      await fn();
    } catch (e) {
      window.alert((e as Error).message);
    } finally {
      setBusy(false);
      reload();
    }
  };
  if (data.sources < 2) return <p className="muted">Transfers appear once two sources are added, for example an exchange and a wallet.</p>;
  return (
    <>
      <h2 className="section-title">Paired</h2>
      {data.pairs.length === 0 ? (
        <p className="muted">No transfer between your sources found yet.</p>
      ) : (
        <table className="table">
          <thead>
            <tr>
              <th>Asset</th>
              <th>From</th>
              <th>To</th>
              <th className="num">Cost carried</th>
              <th>Matched by</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {data.pairs.map((p) => (
              <tr key={p.out.id + p.in.id}>
                <td className="nowrap">
                  <span className="asset-line">
                    <CryptoIcon symbol={p.in.base.asset.symbol} size={18} />
                    <strong>{qty(p.in.base.qty)}</strong> {assetLabel(p.in.base.asset)}
                  </span>
                </td>
                <td>{where(p.out)}</td>
                <td>{where(p.in)}</td>
                <td className="num">{p.cost ? money(p.cost, data.currency) : <span className="muted">unknown</span>}</td>
                <td className="muted small">{BY[p.by]}</td>
                <td className="num">
                  <button
                    className="link small"
                    disabled={busy}
                    onClick={() => act(() => api.putTransfer(p.out.id, p.in.id, p.by === "user" ? "" : "refuse"))}
                  >
                    Not a transfer
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <h2 className="section-title">Left without arriving</h2>
      <p className="muted check-intro">
        Withdrawals Fundarr did not see arrive on another of your sources: sent to someone, to a platform you have not added, or to a wallet whose
        deposit came later or with another amount. Pick the deposit it became if you see it.
      </p>
      {data.unmatched.length === 0 ? (
        <p className="muted">Every withdrawal arrived somewhere you track.</p>
      ) : (
        <table className="table">
          <thead>
            <tr>
              <th>Asset</th>
              <th>From</th>
              <th>Became</th>
            </tr>
          </thead>
          <tbody>
            {data.unmatched.map((u) => (
              <tr key={u.out.id}>
                <td className="nowrap">
                  <span className="asset-line">
                    <CryptoIcon symbol={u.out.base.asset.symbol} size={18} />
                    <strong>{qty(u.out.base.qty)}</strong> {assetLabel(u.out.base.asset)}
                  </span>
                </td>
                <td>{where(u.out)}</td>
                <td>
                  {u.candidates.length === 0 ? (
                    <span className="muted small">no deposit of {assetLabel(u.out.base.asset)} within 45 days</span>
                  ) : (
                    <select
                      disabled={busy}
                      defaultValue=""
                      onChange={(e) => e.target.value && act(() => api.putTransfer(u.out.id, e.target.value, "pair"))}
                    >
                      <option value="">Not a transfer of mine</option>
                      {u.candidates.map((c) => (
                        <option key={c.id} value={c.id}>
                          {qty(c.base.qty)} {assetLabel(c.base.asset)} on {c.source}, {dateTime(c.time)}
                        </option>
                      ))}
                    </select>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}
