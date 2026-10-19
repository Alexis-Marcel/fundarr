import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { api, type Source, type SourceDef } from "../api";
import { KindBadge, StatusDot, type Health } from "../components/Badge";
import { EntityForm } from "../components/EntityForm";
import { Modal } from "../components/Modal";
import { ago, isZeroTime, usePoll } from "../lib";

export function health(s: Source): Health {
  if (s.running) return "running";
  if (!s.enabled) return "idle";
  if (s.lastError) return "error";
  if (s.gaps && s.gaps.length > 0) return "warn";
  if (isZeroTime(s.lastOk)) return "idle";
  return "ok";
}

export function statusText(s: Source): string {
  if (s.running) return isZeroTime(s.lastOk) ? "First sync running…" : "Syncing…";
  if (!s.enabled) return "Paused";
  if (s.lastError) return s.lastError;
  if (isZeroTime(s.lastOk)) return "Waiting for its first sync";
  if (s.gaps && s.gaps.length > 0)
    return `History and balance differ on ${s.gaps.map((g) => g.asset.symbol).join(", ")}`;
  if (s.corrections > 0) return `Matches the balances, with ${s.corrections} correction${s.corrections > 1 ? "s" : ""}`;
  return "History matches the balances";
}

export function SourcesPage({ defs }: { defs: SourceDef[] }) {
  const anyRunning = (list: Source[] | null) => list?.some((s) => s.running) ?? false;
  const [fast, setFast] = useState(false);
  const { data, error, reload } = usePoll(
    async () => {
      const list = await api.sources();
      setFast(anyRunning(list));
      return list;
    },
    fast ? 2500 : 15000,
    [fast],
  );
  const [adding, setAdding] = useState(false);

  return (
    <section className="page">
      <header className="page-head">
        <div>
          <h1>Sources</h1>
          <p className="lede">Exchanges, brokers and wallets Fundarr reads from, with read-only access.</p>
        </div>
        <button className="btn btn-primary" onClick={() => setAdding(true)}>
          Add source
        </button>
      </header>
      {error && <p className="notice notice-error">{error}</p>}
      {data && data.length === 0 && (
        <div className="empty">
          <h2>No source yet</h2>
          <p>Add an exchange with a read-only API key. Fundarr fetches its whole history, then checks it against the balances.</p>
          <button className="btn btn-primary" onClick={() => setAdding(true)}>
            Add your first source
          </button>
        </div>
      )}
      {data && data.length > 0 && (
        <ul className="rows">
          {data.map((s) => (
            <li key={s.id} className={`row ${!s.enabled ? "is-off" : ""}`}>
              <Link to={`/sources/${s.id}`} className="row-link">
                <KindBadge kind={s.kind} />
                <span className="row-main">
                  <strong>{s.name}</strong>
                  <span className={`row-status status-${health(s)}`}>
                    <StatusDot health={health(s)} />
                    <span className="clip">{statusText(s)}</span>
                  </span>
                </span>
                <span className="row-stat">
                  <b>{s.activities.toLocaleString("en")}</b>
                  <small>activities</small>
                </span>
                <span className="row-stat">
                  <b>{ago(s.lastOk)}</b>
                  <small>last sync</small>
                </span>
              </Link>
              <button
                className="btn btn-small"
                disabled={s.running}
                onClick={async () => {
                  await api.sync(s.id).catch(() => {});
                  setFast(true);
                  reload();
                }}
              >
                {s.running ? "Syncing" : "Sync now"}
              </button>
            </li>
          ))}
        </ul>
      )}
      {adding && <AddSource defs={defs} onClose={() => setAdding(false)} onAdded={() => { setAdding(false); setFast(true); reload(); }} />}
    </section>
  );
}

function AddSource({ defs, onClose, onAdded }: { defs: SourceDef[]; onClose: () => void; onAdded: () => void }) {
  const [def, setDef] = useState<SourceDef | null>(null);
  const navigate = useNavigate();
  return (
    <Modal title={def ? `Add ${def.name}` : "Add a source"} onClose={onClose} wide={!def}>
      {!def ? (
        <div className="picker">
          {defs.filter((d) => !d.hidden).map((d) => (
            <button key={d.kind} className="pick" onClick={() => setDef(d)}>
              <KindBadge kind={d.kind} size={44} />
              <span>
                <strong>{d.name}</strong>
                <small>{d.category}</small>
              </span>
            </button>
          ))}
          <p className="muted small picker-note">
            More connectors are on the way, starting with Trade Republic. Each one is a small Go package; see CONTRIBUTING.md.
          </p>
        </div>
      ) : (
        <EntityForm
          kind={def.kind}
          defName={def.name}
          fields={def.fields}
          setup={def.setup}
          saveLabel="Save and sync"
          onTest={async (name, values) => {
            await api.testSource({ kind: def.kind, name, values });
          }}
          onSave={async (name, values) => {
            const s = await api.createSource({ kind: def.kind, name, values });
            onAdded();
            navigate(`/sources/${s.id}`);
          }}
        />
      )}
    </Modal>
  );
}
