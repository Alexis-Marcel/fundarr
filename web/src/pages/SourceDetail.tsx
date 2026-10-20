import { useEffect, useRef, useState } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import { api, type Activity, type Adjustment, type ImportResult, type Login, type Reclassify, type Source, type SourceDef, type Statement } from "../api";
import { KindBadge, StatusDot } from "../components/Badge";
import { EntityForm } from "../components/EntityForm";
import { WalletGuide } from "../components/WalletGuide";
import { AssetName, CryptoIcon } from "../components/CryptoIcon";
import { walletApp } from "../wallets";
import { Modal } from "../components/Modal";
import { RunList } from "./Activity";
import { accountName, ago, assetLabel, badgeKind, dateTime, day, isZeroTime, money, qty, until, usePoll } from "../lib";
import { health, needsSignIn, signingIn, statusText } from "./Sources";

type Tab = "check" | "activities" | "runs";

export function SourceDetail({ defs, autoSync = false }: { defs: SourceDef[]; autoSync?: boolean }) {
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const [params] = useSearchParams();
  const [fast, setFast] = useState(true);
  const { data: src, reload } = usePoll(
    async () => {
      const s = (await api.sources()).find((x) => x.id === id) ?? null;
      setFast(!!s && (s.running || signingIn(s)));
      return s;
    },
    fast ? 2000 : 20000,
    [id, fast],
  );
  const { data: stmts } = usePoll(() => api.statements(id), 60000, [id, src?.lastRun]);
  const [tab, setTab] = useState<Tab>("check");
  const [editing, setEditing] = useState(false);
  const [importing, setImporting] = useState(() => params.get("import") === "1");
  const [syncErr, setSyncErr] = useState<string | null>(null);
  const def = defs.find((d) => d.kind === src?.kind);

  const sync = async (method?: string) => {
    setSyncErr(null);
    try {
      await api.sync(id, false, method);
    } catch (e) {
      setSyncErr((e as Error).message);
    }
    setFast(true);
    reload();
  };
  // /sources/:id/sync (a bookmark on the phone): sync at once, once.
  const auto = useRef(false);
  useEffect(() => {
    if (!autoSync || auto.current || !id) return;
    auto.current = true;
    sync().finally(() => navigate(`/sources/${id}`, { replace: true }));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [autoSync, id]);

  if (!src) return <section className="page"><p className="muted">Loading…</p></section>;
  const lastImport = stmts?.[0];

  return (
    <section className="page">
      <header className="detail-head">
        <KindBadge kind={badgeKind(src)} size={56} />
        <div className="detail-title">
          <h1>{src.name}</h1>
          <p className={`row-status status-${health(src)}`}>
            <StatusDot health={health(src)} />
            <span>{statusText(src)}</span>
          </p>
        </div>
        <div className="detail-actions">
          {src.offline ? (
            def?.statement && (
              <button className="btn btn-primary" disabled={src.running} onClick={() => setImporting(true)}>
                Import statement
              </button>
            )
          ) : (
            <button className="btn btn-primary" disabled={src.running || signingIn(src)} onClick={() => sync()}>
              {src.running ? "Syncing…" : signingIn(src) ? "Waiting…" : "Sync now"}
            </button>
          )}
          {!src.offline && def?.statement && (
            <button className="btn" disabled={src.running} onClick={() => setImporting(true)}>
              Import statement
            </button>
          )}
          <button className="btn" onClick={() => setEditing(true)}>
            Settings
          </button>
        </div>
      </header>

      {syncErr && !src.login?.error && <p className="notice notice-error">{syncErr}</p>}
      {src.session && def && <Connection src={src} def={def} onSync={sync} onChanged={reload} />}

      <dl className="stats">
        {src.offline ? (
          <div>
            <dt>Last import</dt>
            <dd>{lastImport ? ago(lastImport.at) : "None yet"}</dd>
            <small>{lastImport ? `up to ${day(lastImport.to)}` : "CSV export"}</small>
          </div>
        ) : (
          <div>
            <dt>Last sync</dt>
            <dd title={isZeroTime(src.lastOk) ? undefined : dateTime(src.lastOk)}>{src.running ? "Running now" : ago(src.lastOk)}</dd>
            {!isZeroTime(src.lastOk) && <small>{dateTime(src.lastOk)}</small>}
          </div>
        )}
        {src.offline ? (
          <div>
            <dt>Accounts</dt>
            <dd>{1 + (src.subAccounts?.length ?? 0)}</dd>
            <small className="clip">{[accountName(""), ...(src.subAccounts ?? [])].join(", ")}</small>
          </div>
        ) : (
          <div>
            <dt>Next sync</dt>
            <dd>{nextSync(src)}</dd>
            <small>every {src.intervalMinutes >= 60 ? `${src.intervalMinutes / 60} h` : `${src.intervalMinutes} min`}</small>
          </div>
        )}
        <div>
          <dt>Activities</dt>
          <dd>{src.activities.toLocaleString("en")}</dd>
          <small>{def?.name ?? src.kind}</small>
        </div>
        <div>
          <dt>Balance check</dt>
          <dd className={src.gaps && src.gaps.length ? "status-warn" : ""}>
            {isZeroTime(src.balancesAt) || (src.offline && src.activities === 0)
              ? "Pending"
              : src.balancesDerived
                ? "From history"
                : src.gaps && src.gaps.length
                  ? `${src.gaps.length} to explain`
                  : src.corrections > 0
                    ? `${src.corrections} corrected`
                    : "All match"}
          </dd>
          <small>
            {src.offline && src.activities === 0
              ? "after the import"
              : isZeroTime(src.balancesAt)
              ? "after the first sync"
              : src.balancesDerived
                ? src.offline
                  ? "no balances in the export"
                  : "balances not read"
                : `balances ${ago(src.balancesAt)}`}
          </small>
        </div>
      </dl>

      <nav className="tabs" role="tablist">
        {(
          [
            ["check", src.balancesDerived ? "Holdings" : "Balance check"],
            ["activities", `Activities (${src.activities.toLocaleString("en")})`],
            ["runs", src.offline ? "Log" : "Sync log"],
          ] as [Tab, string][]
        ).map(([t, label]) => (
          <button key={t} role="tab" aria-selected={tab === t} className={tab === t ? "is-on" : ""} onClick={() => setTab(t)}>
            {label}
          </button>
        ))}
      </nav>

      {tab === "check" && (
        <>
          <Statements list={stmts} />
          <Check src={src} />
          <Corrections src={src} limits={def?.limits} canImport={!!def?.statement} onImport={() => setImporting(true)} onChanged={reload} />
        </>
      )}
      {tab === "activities" && <Activities id={src.id} running={src.running} />}
      {tab === "runs" && <RunList subject={src.id} />}

      {importing && def?.statement && (
        <ImportStatement
          src={src}
          help={def.statement}
          onClose={() => setImporting(false)}
          onDone={() => {
            reload();
            setTab("check");
          }}
        />
      )}

      {editing && def && (
        <Modal title={`${src.name} settings`} onClose={() => setEditing(false)}>
          <EntityForm
            defName={def.name}
            fields={def.fields}
            setup={def.setup}
            initialName={src.name}
            kind={src.kind}
            sourceId={src.id}
            guide={src.kind === "wallet" ? <WalletGuide app={walletApp(src.settings?.app)} /> : undefined}
            settings={src.settings}
            secretsSet={src.secretsSet}
            secretItems={src.secretItems}
            saveLabel="Save"
            onTest={async (name, values) => {
              await api.testSource({ id: src.id, name, values });
            }}
            onSave={async (name, values) => {
              await api.updateSource(src.id, { name, values });
              setEditing(false);
              reload();
            }}
            extra={<EntitySettings src={src} onChanged={reload} />}
          />
          <div className="danger-zone">
            <p>Fetch the whole history again, as on the first sync. Useful after an update teaches Fundarr new kinds of activity; nothing is duplicated.</p>
            <button
              className="btn"
              disabled={src.running}
              onClick={async () => {
                await api.sync(src.id, true);
                setEditing(false);
                setFast(true);
                reload();
              }}
            >
              Fetch the whole history again
            </button>
            <p>Deleting a source removes its history from Fundarr. Destinations keep what they already received.</p>
            <button
              className="btn btn-danger"
              onClick={async () => {
                if (!window.confirm(`Delete ${src.name} and its history in Fundarr?`)) return;
                await api.deleteSource(src.id);
                navigate("/sources");
              }}
            >
              Delete source
            </button>
          </div>
        </Modal>
      )}
    </section>
  );
}

function Statements({ list: data }: { list: Statement[] | null }) {
  if (!data || data.length === 0) return null;
  return (
    <ul className="statements">
      {data.map((s) => (
        <li key={s.id}>
          <strong>{s.name}</strong>
          <span className="muted">
            {new Date(s.from).toLocaleDateString("en", { dateStyle: "medium" })} to {new Date(s.to).toLocaleDateString("en", { dateStyle: "medium" })},{" "}
            {s.activities.toLocaleString("en")} activities, imported {ago(s.at)}
          </span>
        </li>
      ))}
    </ul>
  );
}

function ImportStatement({ src, help, onClose, onDone }: { src: Source; help: string; onClose: () => void; onDone: () => void }) {
  const [file, setFile] = useState<File | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [res, setRes] = useState<ImportResult | null>(null);
  const run = async () => {
    if (!file) return;
    setBusy(true);
    setErr(null);
    try {
      const buf = new Uint8Array(await file.arrayBuffer());
      let bin = "";
      for (let i = 0; i < buf.length; i += 0x8000) bin += String.fromCharCode(...buf.subarray(i, i + 0x8000));
      setRes(await api.importStatement(src.id, file.name, btoa(bin)));
      onDone();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  return (
    <Modal title={`Import a ${src.name} statement`} onClose={onClose}>
      {!res ? (
        <div className="entity-form">
          <p className="setup">{help}</p>
          <label className="field">
            <span>Statement file</span>
            <input type="file" accept=".csv,text/csv" onChange={(e) => setFile(e.target.files?.[0] ?? null)} />
          </label>
          {err && <p className="notice notice-error">{err}</p>}
          <div className="form-actions">
            <button className="btn btn-primary" disabled={!file || busy} onClick={run}>
              {busy ? "Importing…" : "Import"}
            </button>
          </div>
        </div>
      ) : (
        <div className="entity-form">
          <p className={res.gaps && res.gaps.length ? "notice notice-error" : "notice notice-ok"}>
            {res.activities.toLocaleString("en")} activities from {new Date(res.from).toLocaleDateString("en", { dateStyle: "medium" })} to{" "}
            {new Date(res.to).toLocaleDateString("en", { dateStyle: "medium" })}.{" "}
            {res.gaps && res.gaps.length
              ? `${res.gaps.length} asset${res.gaps.length > 1 ? "s" : ""} still differ: ${res.gaps.map((g) => g.asset.symbol).join(", ")}.`
              : "History and balances now match."}
          </p>
          {res.notes && res.notes.length > 0 && (
            <ul className="notes">
              {res.notes.map((n) => (
                <li key={n} className="muted small">
                  {n}
                </li>
              ))}
            </ul>
          )}
          <div className="form-actions">
            <button className="btn btn-primary" onClick={onClose}>
              Done
            </button>
          </div>
        </div>
      )}
    </Modal>
  );
}

function nextSync(s: Source): string {
  if (s.running) return "After this one";
  if (!s.enabled) return "Paused";
  if (needsSignIn(s)) return "After approval";
  if (isZeroTime(s.lastRun)) return "Within a minute";
  // A failed run is retried after 15 minutes at most.
  const wait = s.lastError ? Math.min(s.intervalMinutes, 15) : s.intervalMinutes;
  const at = new Date(new Date(s.lastRun).getTime() + wait * 60000);
  if (at.getTime() <= Date.now()) return "Within a minute";
  return ago(at.toISOString());
}

function EntitySettings({ src, onChanged }: { src: Source; onChanged: () => void }) {
  const opts = [60, 180, 360, 720, 1440];
  return (
    <div className="inline-settings">
      <label className="field">
        <span>Sync every</span>
        <select
          value={src.intervalMinutes}
          onChange={async (e) => {
            await api.updateSource(src.id, { intervalMinutes: Number(e.target.value) });
            onChanged();
          }}
        >
          {opts.map((m) => (
            <option key={m} value={m}>
              {m < 1440 ? `${m / 60} hour${m > 60 ? "s" : ""}` : "day"}
            </option>
          ))}
        </select>
      </label>
      <label className="check">
        <input
          type="checkbox"
          checked={src.enabled}
          onChange={async (e) => {
            await api.updateSource(src.id, { enabled: e.target.checked });
            onChanged();
          }}
        />
        <span>Sync automatically</span>
      </label>
      <label className="check">
        <input
          type="checkbox"
          checked={src.adjust}
          onChange={async (e) => {
            await api.updateSource(src.id, { adjust: e.target.checked });
            onChanged();
          }}
        />
        <span>Correct balances the history does not explain</span>
      </label>
    </div>
  );
}

function Check({ src }: { src: Source }) {
  const { data } = usePoll(() => api.holdings(src.id), src.running ? 4000 : 30000, [src.id, src.running, src.lastOk]);
  if (!data) return <p className="muted">Loading…</p>;
  if (src.offline && src.activities === 0) return <p className="muted">Import the CSV export to see what it holds.</p>;
  if (isZeroTime(data.balancesAt)) return <p className="muted">Balances arrive at the end of the first sync.</p>;
  if (data.holdings.length === 0)
    return <p className="muted check-intro">Nothing is held here right now, and the history agrees: every coin that came in also went out.</p>;
  const lines = [...data.holdings].sort((a, b) => Number(b.gap) - Number(a.gap) || assetLabel(a.asset).localeCompare(assetLabel(b.asset)));
  if (src.balancesDerived)
    return (
      <div>
        <p className="muted check-intro">What the history adds up to. {src.offline ? "The export has no balances to check it against." : `${src.name} gave no balances this time.`}</p>
        <table className="table">
          <thead>
            <tr>
              <th>Asset</th>
              <th className="num">Held</th>
            </tr>
          </thead>
          <tbody>
            {lines.map((l) => (
              <tr key={l.asset.kind + l.asset.symbol}>
                <td>
                  <AssetName asset={l.asset} /> {l.asset.kind !== "crypto" && <small className="muted">{l.asset.kind}</small>}
                </td>
                <td className="num">{qty(l.replayed)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    );
  return (
    <div>
      <p className="muted check-intro">
        Fundarr replays every activity it fetched and compares the result with what {src.name} reports. When they match, the history is complete
        and destinations get the right quantities. Balances as of {dateTime(data.balancesAt)}.
      </p>
      <table className="table">
        <thead>
          <tr>
            <th>Asset</th>
            <th className="num">From history</th>
            <th className="num">Correction</th>
            <th className="num">Reported</th>
            <th>Check</th>
          </tr>
        </thead>
        <tbody>
          {lines.map((l) => (
            <tr key={l.asset.kind + l.asset.symbol} className={l.gap ? "is-gap" : ""}>
              <td>
                <AssetName asset={l.asset} /> {l.asset.kind !== "crypto" && <small className="muted">{l.asset.kind}</small>}
              </td>
              <td className="num">{qty(l.replayed)}</td>
              <td className="num muted">{Number(l.correction) === 0 ? "" : (Number(l.correction) > 0 ? "+" : "") + qty(l.correction)}</td>
              <td className="num">{qty(l.reported)}</td>
              <td>
                {l.gap ? (
                  <span className="tag tag-warn">differs</span>
                ) : Number(l.correction) !== 0 ? (
                  <span className="tag tag-corrected">corrected</span>
                ) : (
                  <span className="tag tag-ok">matches</span>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

const TYPE_LABEL: Record<string, string> = {
  adjustment: "Correction",
  trade: "Trade",
  deposit: "Deposit",
  withdrawal: "Withdrawal",
  reward: "Reward",
  dividend: "Dividend",
  interest: "Interest",
  fee: "Fee",
  tax: "Tax",
};

function describe(a: Activity) {
  const b = `${qty(a.base.qty)} ${assetLabel(a.base.asset)}`;
  if (a.type === "trade" && a.quote) {
    const q = `${qty(a.quote.qty)} ${assetLabel(a.quote.asset)}`;
    return a.side === "buy" ? `Bought ${b} for ${q}` : `Sold ${b} for ${q}`;
  }
  if (a.type === "dividend" && a.quote) return `${b} from ${assetLabel(a.quote.asset)}`;
  if (a.type === "adjustment") return a.side === "in" ? `+${b}` : `−${b}`;
  return b;
}

function Activities({ id, running }: { id: string; running: boolean }) {
  const [page, setPage] = useState(0);
  const size = 50;
  const { data, reload } = usePoll(() => api.activities(id, page * size, size), running ? 5000 : 60000, [id, page, running]);
  if (!data) return <p className="muted">Loading…</p>;
  if (data.total === 0) return <p className="muted">No activity yet.</p>;
  const pages = Math.ceil(data.total / size);
  return (
    <div>
      <table className="table">
        <thead>
          <tr>
            <th>Date</th>
            <th>Type</th>
            <th>What</th>
            <th className="num">Fee</th>
          </tr>
        </thead>
        <tbody>
          {data.activities.map((a) => (
            <tr key={a.id}>
              <td className="nowrap">{dateTime(a.time)}</td>
              <td>
                <span className={`tag tag-type tag-${a.type}`}>{a.type === "trade" ? (a.side === "buy" ? "Buy" : "Sell") : (TYPE_LABEL[a.type] ?? a.type)}</span>
              </td>
              <td>
                <span className="asset-line">
                  <CryptoIcon symbol={a.base.asset.symbol || a.base.asset.isin} size={18} />
                  {describe(a)}
                </span>
                {a.nativeId.startsWith("manual:") && (
                  <>
                    {" "}
                    <span className="tag tag-entered">entered by you</span>{" "}
                    <button
                      className="link small"
                      disabled={running}
                      onClick={async () => {
                        if (!window.confirm("Delete this activity? The balance check corrects again what it explained.")) return;
                        await api.deleteManual(id, a.id).catch((e) => window.alert((e as Error).message));
                        reload();
                      }}
                    >
                      Delete
                    </button>
                  </>
                )}
                {a.note && <small className="muted note">{a.note}</small>}
              </td>
              <td className="num">{a.fee ? `${qty(a.fee.qty)} ${assetLabel(a.fee.asset)}` : ""}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {pages > 1 && (
        <div className="pager">
          <button className="btn btn-small" disabled={page === 0} onClick={() => setPage(page - 1)}>
            Newer
          </button>
          <span className="muted small">
            Page {page + 1} of {pages}
          </span>
          <button className="btn btn-small" disabled={page + 1 >= pages} onClick={() => setPage(page + 1)}>
            Older
          </button>
        </div>
      )}
    </div>
  );
}

/** Balance corrections: what they are, when they go out, what they really were. */
function Corrections({
  src,
  limits,
  canImport,
  onImport,
  onChanged,
}: {
  src: Source;
  limits?: string;
  canImport: boolean;
  onImport: () => void;
  onChanged: () => void;
}) {
  const { data, reload } = usePoll(() => api.adjustments(src.id), 60000, [src.id, src.lastOk, src.corrections]);
  const [editing, setEditing] = useState<Adjustment | null>(null);
  if (!data || data.length === 0) {
    if (!src.adjust && src.gaps && src.gaps.length > 0)
      return <p className="muted check-intro">Balance corrections are off for this source (Settings): the differences above stay unexplained in destinations.</p>;
    return null;
  }
  const ccy = data[0].currency;
  return (
    <section className="corrections">
      <h2>Corrections</h2>
      <p className="muted check-intro">
        {limits ? limits + " " : ""}A correction adds or removes what the history does not explain, valued at that day&apos;s price, so balances and
        totals are right. It waits 48 hours before going to destinations, in case the history catches up. When you know what it really was, say so
        and it becomes that activity.
      </p>
      {canImport && (
        <p className="check-intro">
          <button className="btn btn-small" disabled={src.running} onClick={onImport}>
            Import statement
          </button>
        </p>
      )}
      <table className="table">
        <thead>
          <tr>
            <th>Date</th>
            <th>Asset</th>
            <th className="num">Quantity</th>
            <th className="num">Value</th>
            <th>Status</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {data.map((a) => (
            <tr key={a.id}>
              <td className="nowrap">{day(a.time)}</td>
              <td>
                <AssetName asset={a.base.asset} />
                <small className="muted note">{a.note}</small>
              </td>
              <td className="num">
                {a.side === "in" ? "+" : "−"}
                {qty(a.base.qty)}
              </td>
              <td className="num">{a.value ? money(a.value, ccy) : <span className="muted">no price</span>}</td>
              <td className="nowrap">
                {a.delivered ? (
                  <span className="tag tag-ok">sent</span>
                ) : new Date(a.releaseAt).getTime() > Date.now() ? (
                  <span className="muted small">goes out {ago(a.releaseAt)}</span>
                ) : (
                  <span className="muted small">ready</span>
                )}
              </td>
              <td className="num">
                {!a.delivered && (
                  <button className="btn btn-small" disabled={src.running} onClick={() => setEditing(a)}>
                    It was…
                  </button>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {editing && (
        <ReclassifyModal
          src={src}
          adj={editing}
          onClose={() => setEditing(null)}
          onDone={() => {
            setEditing(null);
            reload();
            onChanged();
          }}
        />
      )}
    </section>
  );
}

function ReclassifyModal({ src, adj, onClose, onDone }: { src: Source; adj: Adjustment; onClose: () => void; onDone: () => void }) {
  const incoming = adj.side === "in";
  const kinds: [Reclassify["as"], string][] = incoming
    ? [
        ["buy", "A purchase"],
        ["reward", "A reward, airdrop or gift"],
        ["deposit", "A deposit from another wallet"],
      ]
    : [
        ["sell", "A sale"],
        ["withdrawal", "A withdrawal to another wallet"],
        ["fee", "A fee"],
      ];
  const [as, setAs] = useState<Reclassify["as"]>(kinds[0][0]);
  const [date, setDate] = useState(adj.time.slice(0, 10));
  const [qsym, setQsym] = useState("EUR");
  const [qqty, setQqty] = useState("");
  const [note, setNote] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const trade = as === "buy" || as === "sell";
  const save = async () => {
    setBusy(true);
    setErr(null);
    try {
      const time = date === adj.time.slice(0, 10) ? undefined : new Date(date + "T12:00:00Z").toISOString();
      await api.reclassify(src.id, adj.id, {
        as,
        time,
        note: note || undefined,
        quote: trade ? { asset: { symbol: qsym.trim().toUpperCase() }, qty: qqty.trim().replace(",", ".") } : undefined,
      });
      onDone();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  return (
    <Modal title={`${incoming ? "+" : "−"}${qty(adj.base.qty)} ${assetLabel(adj.base.asset)}: what was it?`} onClose={onClose}>
      <div className="entity-form">
        <fieldset className="choices">
          {kinds.map(([k, label]) => (
            <label key={k} className="check">
              <input type="radio" name="as" checked={as === k} onChange={() => setAs(k)} />
              <span>{label}</span>
            </label>
          ))}
        </fieldset>
        {trade && (
          <div className="field-row">
            <label className="field">
              <span>{as === "buy" ? "Paid" : "Received"}</span>
              <input inputMode="decimal" value={qqty} onChange={(e) => setQqty(e.target.value)} placeholder="1000" required />
            </label>
            <label className="field">
              <span>In</span>
              <input value={qsym} onChange={(e) => setQsym(e.target.value)} placeholder="EUR, USDT…" />
            </label>
          </div>
        )}
        <label className="field">
          <span>Date</span>
          <input type="date" value={date} onChange={(e) => setDate(e.target.value)} />
          <small>{incoming ? "When it arrived, if you know it better." : "When it left, if you know it better."}</small>
        </label>
        <label className="field">
          <span>Note</span>
          <input value={note} onChange={(e) => setNote(e.target.value)} placeholder="Optional" />
        </label>
        {trade && qsym.trim().toUpperCase() !== "" && (
          <p className="muted small">
            If {src.name}&apos;s history does not show the {qsym.trim().toUpperCase()} {as === "buy" ? "paid" : "received"} either, a new correction
            covers it.
          </p>
        )}
        {err && <p className="notice notice-error">{err}</p>}
        <div className="form-actions">
          <button className="btn btn-primary" disabled={busy || (trade && !qqty.trim())} onClick={save}>
            {busy ? "Saving…" : "Save"}
          </button>
        </div>
      </div>
    </Modal>
  );
}

/** The sign-in of a source that keeps a session: its state, and the steps of a sign-in. */
function Connection({ src, def, onSync, onChanged }: { src: Source; def: SourceDef; onSync: (method?: string) => Promise<void>; onChanged: () => void }) {
  const [copied, setCopied] = useState(false);
  const login = src.login;
  if (login && signingIn(src)) return <SignIn src={src} def={def} login={login} onSync={onSync} onChanged={onChanged} />;
  const link = `${window.location.origin}/sources/${src.id}/sync`;
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(link);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 2000);
    } catch {
      window.prompt("Sync link", link);
    }
  };
  return (
    <div className={`connection ${login?.error ? "is-error" : ""}`}>
      {login?.error ? (
        <>
          <StatusDot health="error" />
          <span className="connection-text">
            <strong>Sign-in failed</strong>
            <span className="muted small">{login.error}</span>
          </span>
          <button className="btn btn-small" onClick={() => onSync()}>
            Try again
          </button>
        </>
      ) : login?.done ? (
        <>
          <StatusDot health="running" />
          <span className="connection-text">
            <strong>Signed in</strong>
            <span className="muted small">Syncing…</span>
          </span>
        </>
      ) : needsSignIn(src) ? (
        <>
          <StatusDot health="warn" />
          <span className="connection-text">
            <strong>Approve to sync</strong>
            <span className="muted small">Sync asks for it in your {def.name} app.</span>
          </span>
        </>
      ) : (
        <>
          <StatusDot health="ok" />
          <span className="connection-text">
            <strong>Connected until {until(src.session!.until!)}</strong>
            <span className="muted small">Syncs need no approval until then.</span>
          </span>
        </>
      )}
      <button className="link small" onClick={copy} title="Bookmark it on your phone: one tap syncs">
        {copied ? "Copied" : "Copy sync link"}
      </button>
    </div>
  );
}

function useCountdown(expires?: string) {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const t = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(t);
  }, []);
  if (!expires) return 0;
  return Math.max(0, Math.round((new Date(expires).getTime() - now) / 1000));
}

function SignIn({ src, def, login, onSync, onChanged }: { src: Source; def: SourceDef; login: Login; onSync: (method?: string) => Promise<void>; onChanged: () => void }) {
  const c = login.challenge;
  const left = useCountdown(c?.expires);
  const total = c ? Math.max(1, (new Date(c.expires).getTime() - new Date(login.started).getTime()) / 1000) : 1;
  const [code, setCode] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const methods = src.session?.methods ?? [];
  const send = async (v: string) => {
    setBusy(true);
    setErr(null);
    try {
      await api.loginCode(src.id, v);
      onChanged();
    } catch (e) {
      setErr((e as Error).message);
      setCode("");
    } finally {
      setBusy(false);
    }
  };
  const switchTo = async (method: string) => {
    await api.cancelLogin(src.id).catch(() => {});
    await onSync(method);
  };
  const cancel = async () => {
    await api.cancelLogin(src.id).catch(() => {});
    onChanged();
  };
  const mmss = `${Math.floor(left / 60)}:${String(left % 60).padStart(2, "0")}`;
  return (
    <div className="signin" role="status">
      {c?.kind === "qr" && login.qr ? (
        <div className="signin-qr" aria-label="QR code" dangerouslySetInnerHTML={{ __html: login.qr }} />
      ) : (
        <svg className="signin-icon" viewBox="0 0 24 24" aria-hidden width="40" height="40">
          <rect x="6" y="2.5" width="12" height="19" rx="2.5" fill="none" stroke="currentColor" strokeWidth="1.6" />
          <path d="M10.5 18.5h3" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
          <circle cx="17.5" cy="5.5" r="3" className="signin-ping" />
        </svg>
      )}
      <div className="signin-main">
        <strong>
          {c?.kind === "code" ? "Code from your authenticator app" : c?.kind === "qr" ? `Scan with your ${def.name} app` : `Approve in your ${def.name} app`}
        </strong>
        {c?.kind === "code" && (
          <input
            className="code-input"
            inputMode="numeric"
            autoComplete="one-time-code"
            pattern="[0-9]*"
            maxLength={c.digits ?? 6}
            autoFocus
            disabled={busy}
            value={code}
            aria-label="Code"
            onChange={(e) => {
              const v = e.target.value.replace(/\D/g, "");
              setCode(v);
              if (v.length === (c.digits ?? 6)) send(v);
            }}
          />
        )}
        {err && <span className="status-error small">{err}</span>}
        <span className="signin-time">
          <span className="signin-bar">
            <span style={{ width: `${Math.min(100, (left / total) * 100)}%` }} />
          </span>
          <span className="muted small">{mmss}</span>
        </span>
        <span className="signin-links">
          {c?.kind !== "qr" && methods.includes("qr") && (
            <button className="link small" onClick={() => switchTo("qr")}>
              Use a QR code
            </button>
          )}
          {c?.kind === "qr" && methods.includes("app") && (
            <button className="link small" onClick={() => switchTo("app")}>
              Use a notification
            </button>
          )}
          <button className="link small" onClick={cancel}>
            Cancel
          </button>
        </span>
      </div>
    </div>
  );
}
