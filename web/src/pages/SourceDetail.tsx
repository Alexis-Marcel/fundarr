import { useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { api, type Activity, type Source, type SourceDef } from "../api";
import { KindBadge, StatusDot } from "../components/Badge";
import { EntityForm } from "../components/EntityForm";
import { Modal } from "../components/Modal";
import { RunList } from "./Activity";
import { ago, assetLabel, dateTime, isZeroTime, qty, usePoll } from "../lib";
import { health, statusText } from "./Sources";

type Tab = "check" | "activities" | "runs";

export function SourceDetail({ defs }: { defs: SourceDef[] }) {
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const [fast, setFast] = useState(true);
  const { data: src, reload } = usePoll(
    async () => {
      const s = (await api.sources()).find((x) => x.id === id) ?? null;
      setFast(s?.running ?? false);
      return s;
    },
    fast ? 2500 : 20000,
    [id, fast],
  );
  const [tab, setTab] = useState<Tab>("check");
  const [editing, setEditing] = useState(false);
  const def = defs.find((d) => d.kind === src?.kind);

  if (!src) return <section className="page"><p className="muted">Loading…</p></section>;

  return (
    <section className="page">
      <header className="detail-head">
        <KindBadge kind={src.kind} size={56} />
        <div className="detail-title">
          <h1>{src.name}</h1>
          <p className={`row-status status-${health(src)}`}>
            <StatusDot health={health(src)} />
            <span>{statusText(src)}</span>
          </p>
          <p className="muted small">
            {def?.name ?? src.kind}, last synced {ago(src.lastOk)}, syncs every {src.intervalMinutes >= 60 ? `${src.intervalMinutes / 60} h` : `${src.intervalMinutes} min`}
          </p>
        </div>
        <div className="detail-actions">
          <button
            className="btn btn-primary"
            disabled={src.running}
            onClick={async () => {
              await api.sync(src.id).catch(() => {});
              setFast(true);
              reload();
            }}
          >
            {src.running ? "Syncing…" : "Sync now"}
          </button>
          <button className="btn" onClick={() => setEditing(true)}>
            Settings
          </button>
        </div>
      </header>

      <nav className="tabs" role="tablist">
        {(
          [
            ["check", "Balance check"],
            ["activities", `Activities (${src.activities.toLocaleString("en")})`],
            ["runs", "Sync log"],
          ] as [Tab, string][]
        ).map(([t, label]) => (
          <button key={t} role="tab" aria-selected={tab === t} className={tab === t ? "is-on" : ""} onClick={() => setTab(t)}>
            {label}
          </button>
        ))}
      </nav>

      {tab === "check" && <Check src={src} />}
      {tab === "activities" && <Activities id={src.id} running={src.running} />}
      {tab === "runs" && <RunList subject={src.id} />}

      {editing && def && (
        <Modal title={`${src.name} settings`} onClose={() => setEditing(false)}>
          <EntityForm
            defName={def.name}
            fields={def.fields}
            setup={def.setup}
            initialName={src.name}
            settings={src.settings}
            secretsSet={src.secretsSet}
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
    </div>
  );
}

function Check({ src }: { src: Source }) {
  const { data } = usePoll(() => api.holdings(src.id), src.running ? 4000 : 30000, [src.id, src.running, src.lastOk]);
  if (!data) return <p className="muted">Loading…</p>;
  if (isZeroTime(data.balancesAt)) return <p className="muted">Balances arrive at the end of the first sync.</p>;
  const lines = [...data.holdings].sort((a, b) => Number(b.gap) - Number(a.gap) || assetLabel(a.asset).localeCompare(assetLabel(b.asset)));
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
            <th className="num">Reported</th>
            <th>Check</th>
          </tr>
        </thead>
        <tbody>
          {lines.map((l) => (
            <tr key={l.asset.kind + l.asset.symbol} className={l.gap ? "is-gap" : ""}>
              <td>
                <strong>{assetLabel(l.asset)}</strong> <small className="muted">{l.asset.kind}</small>
              </td>
              <td className="num">{qty(l.replayed)}</td>
              <td className="num">{qty(l.reported)}</td>
              <td>{l.gap ? <span className="tag tag-warn">differs</span> : <span className="tag tag-ok">matches</span>}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

const TYPE_LABEL: Record<string, string> = {
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
  return b;
}

function Activities({ id, running }: { id: string; running: boolean }) {
  const [page, setPage] = useState(0);
  const size = 50;
  const { data } = usePoll(() => api.activities(id, page * size, size), running ? 5000 : 60000, [id, page, running]);
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
                <span className={`tag tag-type tag-${a.type}`}>{a.type === "trade" ? (a.side === "buy" ? "Buy" : "Sell") : TYPE_LABEL[a.type] ?? a.type}</span>
              </td>
              <td>
                {describe(a)}
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
