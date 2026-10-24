import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { api, type Source, type SourceDef } from "../api";
import { KindBadge, StatusDot, type Health } from "../components/Badge";
import { EntityForm } from "../components/EntityForm";
import { WalletGuide } from "../components/WalletGuide";
import { otherApp, walletApps, type WalletApp } from "../wallets";
import { Modal } from "../components/Modal";
import { ago, badgeKind, isZeroTime, usePoll } from "../lib";

/** The source waits for a sign-in to sync. */
export function needsSignIn(s: Source): boolean {
  if (!s.session?.needed) return false;
  if (s.lastError.includes("sign-in needed")) return true;
  return !s.session.until || new Date(s.session.until).getTime() <= Date.now();
}

/** A sign-in waits for the user. */
export function signingIn(s: Source): boolean {
  return !!s.login && !s.login.done && !s.login.error;
}

export function health(s: Source): Health {
  if (s.running || signingIn(s)) return "running";
  if (!s.enabled) return "idle";
  if (needsSignIn(s)) return "warn";
  if (s.offline && s.activities === 0) return "idle";
  if (s.lastError) return "error";
  if (s.gaps && s.gaps.length > 0) return "warn";
  if (isZeroTime(s.lastOk)) return "idle";
  return "ok";
}

export function statusText(s: Source): string {
  if (s.running) return isZeroTime(s.lastOk) ? "First sync running…" : "Syncing…";
  if (signingIn(s)) return "Waiting for your approval";
  if (needsSignIn(s)) return "Approve to sync";
  if (s.offline && s.activities === 0) return "Import the CSV export";
  if (s.offline) return "From the imported export";
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
                <KindBadge kind={badgeKind(s)} />
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
                  <small>{s.offline ? "updated" : "last sync"}</small>
                </span>
              </Link>
              {s.offline ? (
                <Link className="btn btn-small" to={`/sources/${s.id}?import=1`}>
                  Import
                </Link>
              ) : s.session ? (
                <Link className="btn btn-small" to={`/sources/${s.id}/sync`}>
                  {s.running ? "Syncing" : "Sync now"}
                </Link>
              ) : (
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
              )}
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
  // A Wallet first asks which one: the guide follows the app.
  const [app, setApp] = useState<WalletApp | null>(null);
  const navigate = useNavigate();
  const wallet = def?.kind === "wallet";
  const title = !def ? "Add a source" : wallet && !app ? "Which wallet?" : `Add ${app?.name ?? def.name}`;
  return (
    <Modal title={title} onClose={onClose} wide={!def || (wallet && !app)}>
      {def && wallet && !app ? (
        <div className="picker">
          {[...walletApps, otherApp].map((a) => (
            <button key={a.id} className="pick" onClick={() => setApp(a)}>
              <KindBadge kind={a.id} size={44} />
              <span>
                <strong>{a.id === "other" ? "Another wallet" : a.name}</strong>
                <small>{a.id === "other" ? "any" : a.kind === "hardware" ? "hardware" : "app"}</small>
              </span>
            </button>
          ))}
          <p className="muted small picker-note">
            Only changes the guide.{" "}
            <button className="link small" onClick={() => setDef(null)}>
              Back
            </button>
          </p>
        </div>
      ) : !def ? (
        <div className="picker">
          {defs.filter((d) => !d.hidden).map((d) => (
            <button key={d.kind} className="pick" onClick={() => setDef(d)}>
              <KindBadge kind={d.kind} size={44} />
              <span>
                <strong>
                  {d.name}
                  {d.beta && <span className="beta">beta</span>}
                </strong>
                <small>{d.category}</small>
              </span>
            </button>
          ))}
        </div>
      ) : (
        <EntityForm
          kind={def.kind}
          defName={app && app.id !== "other" ? app.name : def.name}
          fields={def.fields}
          setup={def.setup}
          modes={def.modes}
          settings={app ? { app: app.id } : undefined}
          guide={app ? <WalletGuide app={app} /> : undefined}
          saveLabel={(mode) => (mode === "connect" ? "Save and connect" : def.modes?.length && def.statement ? "Save and import" : "Save and sync")}
          onTest={
            def.noTest
              ? undefined
              : async (name, values) => {
                  await api.testSource({ kind: def.kind, name, values });
                }
          }
          onSave={async (name, values) => {
            const s = await api.createSource({ kind: def.kind, name, values, skipTest: def.noTest });
            onAdded();
            // Next step right away: the sign-in, or the file to import.
            navigate(s.session ? `/sources/${s.id}/sync` : s.offline && def.statement ? `/sources/${s.id}?import=1` : `/sources/${s.id}`);
          }}
        />
      )}
    </Modal>
  );
}
