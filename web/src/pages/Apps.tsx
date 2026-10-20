import { useEffect, useRef, useState } from "react";
import { api, type APIToken, type DestAccount, type DestDef, type Destination, type Route, type Session, type Source } from "../api";
import { KindBadge, StatusDot } from "../components/Badge";
import { EntityForm } from "../components/EntityForm";
import { Modal } from "../components/Modal";
import { ago, day, isZeroTime, usePoll } from "../lib";

/** A read app: Fundarr does not push to it, it reads /api/v1 with a token. */
type ReadKind = "cairn" | "other";

export function AppsPage({ defs, session }: { defs: DestDef[]; session: Session }) {
  const { data, reload } = usePoll(async () => {
    const [d, s, tokens] = await Promise.all([api.destinations(), api.sources(), api.tokens()]);
    return { ...d, sources: s, tokens };
  }, 8000);
  const [adding, setAdding] = useState(false);

  return (
    <section className="page">
      <header className="page-head">
        <div>
          <h1>Apps</h1>
          <p className="lede">Trackers Fundarr sends your history to, and apps that read it.</p>
        </div>
        <button className="btn btn-primary" onClick={() => setAdding(true)}>
          Add app
        </button>
      </header>
      {data && data.destinations.length === 0 && data.tokens.length === 0 && (
        <div className="empty">
          <h2>No app yet</h2>
          <p>Connect a tracker such as Wealthfolio, or let an app such as Cairn read your portfolio.</p>
          <button className="btn btn-primary" onClick={() => setAdding(true)}>
            Add an app
          </button>
        </div>
      )}
      {data?.destinations.map((d) => (
        <DestinationCard key={d.id} d={d} def={defs.find((x) => x.kind === d.kind)} routes={data.routes.filter((r) => r.destinationId === d.id)} sources={data.sources} onChanged={reload} />
      ))}
      {data?.tokens.map((t) => <ReadAppCard key={t.id} t={t} onChanged={reload} />)}
      {adding && <AddApp defs={defs} session={session} onClose={() => setAdding(false)} onAdded={reload} />}
    </section>
  );
}

const readApps: { kind: ReadKind; name: string; blurb: string }[] = [
  { kind: "cairn", name: "Cairn", blurb: "Portfolio and French tax" },
  { kind: "other", name: "Other app", blurb: "Reads with a token" },
];

/** The badge of a read app: Cairn's own, or the name's initials. */
function readBadge(name: string) {
  return name.trim().toLowerCase() === "cairn" ? "cairn" : name.trim().toLowerCase() || "app";
}

function AddApp({ defs, session, onClose, onAdded }: { defs: DestDef[]; session: Session; onClose: () => void; onAdded: () => void }) {
  const [def, setDef] = useState<DestDef | null>(null);
  const [read, setRead] = useState<ReadKind | null>(null);
  const [made, setMade] = useState<{ name: string; token: string } | null>(null);
  const title = made ? `${made.name} can read Fundarr` : def ? `Add ${def.name}` : read === "cairn" ? "Add Cairn" : "Add an app";
  return (
    <Modal title={title} onClose={onClose} wide={!def && !read}>
      {made ? (
        <ReadAppMade made={made} cairn={read === "cairn"} url={session.url} onDone={onClose} />
      ) : def ? (
        <EntityForm
          defName={def.name}
          fields={def.fields}
          setup={def.setup}
          saveLabel="Connect"
          onSave={async (name, values) => {
            await api.createDestination({ kind: def.kind, name, values });
            onAdded();
            onClose();
          }}
        />
      ) : read ? (
        <EntityForm
          defName={read === "cairn" ? "Cairn" : ""}
          fields={[]}
          saveLabel="Create token"
          onSave={async (name) => {
            const r = await api.createToken(name);
            setMade({ name: r.info.name, token: r.token });
            onAdded();
          }}
        />
      ) : (
        <div className="picker">
          {defs.map((d) => (
            <button key={d.kind} className="pick" onClick={() => setDef(d)}>
              <KindBadge kind={d.kind} size={44} />
              <span>
                <strong>{d.name}</strong>
                <small className="as-is">Fundarr sends to it</small>
              </span>
            </button>
          ))}
          {readApps.map((a) => (
            <button key={a.kind} className="pick" onClick={() => setRead(a.kind)}>
              <KindBadge kind={a.kind === "cairn" ? "cairn" : "app"} size={44} />
              <span>
                <strong>{a.name}</strong>
                <small className="as-is">{a.blurb}</small>
              </span>
            </button>
          ))}
        </div>
      )}
    </Modal>
  );
}

/** What a read app needs, shown once: the API's address and the token. */
function ReadAppMade({ made, cairn, url, onDone }: { made: { name: string; token: string }; cairn: boolean; url: string; onDone: () => void }) {
  const base = `${(url || window.location.origin).replace(/\/+$/, "")}/api/v1`;
  return (
    <div className="entity-form">
      {cairn && <p className="setup">In Cairn: Réglages → Synchroniser avec Fundarr.</p>}
      <CopyField label="API URL" value={base} />
      <p className="muted small">
        App in Docker next to Fundarr? Use <code>http://fundarr:9494/api/v1</code>: a proxy in front of Fundarr may block it.
      </p>
      <CopyField label="Token" value={made.token} />
      <CopyField label="Try it" value={`curl -H "Authorization: Bearer ${made.token}" ${base}/portfolio`} code />
      <p className="muted small">The token is shown only now. Keep it on a server, never in a web page.</p>
      <div className="form-actions">
        <button className="btn btn-primary" onClick={onDone}>
          Done
        </button>
      </div>
    </div>
  );
}

function CopyField({ label, value, code }: { label: string; value: string; code?: boolean }) {
  const [copied, setCopied] = useState(false);
  const ref = useRef<HTMLElement>(null);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value);
    } catch {
      // Not a secure context (plain http on a LAN): select it instead.
      const r = document.createRange();
      r.selectNodeContents(ref.current!);
      window.getSelection()?.removeAllRanges();
      window.getSelection()?.addRange(r);
      if (!document.execCommand("copy")) return;
    }
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1500);
  };
  return (
    <div className="field">
      <span>{label}</span>
      <div className="copy-row">
        <code ref={ref} className={`copy-value ${code ? "is-code" : ""}`}>
          {value}
        </code>
        <button type="button" className="btn btn-small" onClick={copy}>
          {copied ? "Copied" : "Copy"}
        </button>
      </div>
    </div>
  );
}

function ReadAppCard({ t, onChanged }: { t: APIToken; onChanged: () => void }) {
  const used = !isZeroTime(t.lastUsed);
  const [notifying, setNotifying] = useState(false);
  let host = "";
  try {
    host = t.notifyUrl ? new URL(t.notifyUrl).host : "";
  } catch {
    host = t.notifyUrl;
  }
  return (
    <article className="card">
      <header className="card-head">
        <KindBadge kind={readBadge(t.name)} />
        <div className="card-title">
          <h2>{t.name}</h2>
          <p className={`row-status status-${used ? "ok" : "idle"}`}>
            <StatusDot health={used ? "ok" : "idle"} />
            <span className="clip">
              Reads Fundarr · added {day(t.created)} · {used ? `used ${ago(t.lastUsed)}` : "not used yet"}
              {host && ` · notifies ${host}`}
            </span>
          </p>
        </div>
        <div className="detail-actions">
          <button className="btn" onClick={() => setNotifying(true)}>
            Notify
          </button>
          <button
            className="btn"
            onClick={async () => {
              if (!window.confirm(`Remove ${t.name}? Its token stops working.`)) return;
              await api.deleteToken(t.id);
              onChanged();
            }}
          >
            Remove
          </button>
        </div>
      </header>
      {notifying && (
        <Modal title={`Notify ${t.name}`} onClose={() => setNotifying(false)}>
          <NotifyForm
            t={t}
            onSaved={onChanged}
            onDone={() => {
              setNotifying(false);
              onChanged();
            }}
          />
        </Modal>
      )}
    </article>
  );
}

/** Where Fundarr posts when the history changes; the secret is shown once. */
function NotifyForm({ t, onSaved, onDone }: { t: APIToken; onSaved: () => void; onDone: () => void }) {
  const [url, setUrl] = useState(t.notifyUrl);
  const [secret, setSecret] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const save = async (u: string) => {
    setBusy(true);
    setErr("");
    try {
      const r = await api.setNotify(t.id, u);
      onSaved();
      if (r.secret) setSecret(r.secret);
      else onDone();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  if (secret)
    return (
      <div className="entity-form">
        <CopyField label="Secret" value={secret} />
        <p className="muted small">Signs each call (Fundarr-Signature header). Shown only now.</p>
        <div className="form-actions">
          <button className="btn btn-primary" onClick={onDone}>
            Done
          </button>
        </div>
      </div>
    );
  return (
    <form
      className="entity-form"
      onSubmit={(e) => {
        e.preventDefault();
        save(url.trim());
      }}
    >
      <p className="setup">Fundarr calls this address when your history changes.</p>
      <label className="field">
        <span>Notify URL</span>
        <input type="url" value={url} onChange={(e) => setUrl(e.target.value)} placeholder="https://app.example.com/fundarr" maxLength={500} autoFocus />
      </label>
      {err && (
        <p className="notice notice-error" role="status">
          {err}
        </p>
      )}
      <div className="form-actions">
        {t.notifyUrl && (
          <button type="button" className="btn" disabled={busy} onClick={() => save("")}>
            Stop
          </button>
        )}
        <button className="btn btn-primary" disabled={busy || !url.trim()}>
          {busy ? "Saving…" : "Save"}
        </button>
      </div>
    </form>
  );
}

function DestinationCard({ d, def, routes, sources, onChanged }: { d: Destination; def?: DestDef; routes: Route[]; sources: Source[]; onChanged: () => void }) {
  const [accounts, setAccounts] = useState<DestAccount[] | null>(null);
  const [accErr, setAccErr] = useState<string | null>(null);
  const [editing, setEditing] = useState(false);
  useEffect(() => {
    api.accounts(d.id).then(setAccounts, (e) => setAccErr((e as Error).message));
  }, [d.id, d.lastError]);

  const delivered = (d.counts.delivered ?? 0) + (d.counts.duplicate ?? 0);
  const failed = d.counts.failed ?? 0;
  const h = d.running ? "running" : d.lastError || failed ? "error" : delivered ? "ok" : "idle";

  return (
    <article className="card">
      <header className="card-head">
        <KindBadge kind={d.kind} />
        <div className="card-title">
          <h2>{d.name}</h2>
          <p className={`row-status status-${h}`}>
            <StatusDot health={h} />
            <span className="clip">
              {d.running
                ? "Delivering…"
                : d.lastError
                  ? d.lastError
                  : `${delivered.toLocaleString("en")} delivered${failed ? `, ${failed} waiting for a retry` : ""}${d.counts.skipped ? `, ${d.counts.skipped} not representable` : ""}`}
            </span>
          </p>
        </div>
        <div className="detail-actions">
          <button
            className="btn"
            disabled={d.running || routes.length === 0}
            onClick={async () => {
              await api.push(d.id).catch(() => {});
              onChanged();
            }}
          >
            Deliver now
          </button>
          <button className="btn" onClick={() => setEditing(true)}>
            Settings
          </button>
        </div>
      </header>

      {accErr && <p className="notice notice-error">Cannot list accounts: {accErr}</p>}
      {sources.length === 0 ? (
        <p className="muted">Add a source first; then choose here which account it goes to.</p>
      ) : (
        <table className="table mapping">
          <thead>
            <tr>
              <th>Source</th>
              <th>Goes to account</th>
              <th>From date</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {sources.map((s) => (
              <MappingRow key={s.id} d={d} s={s} route={routes.find((r) => r.sourceId === s.id)} accounts={accounts} onChanged={onChanged} />
            ))}
          </tbody>
        </table>
      )}

      {editing && def && (
        <Modal title={`${d.name} settings`} onClose={() => setEditing(false)}>
          <EntityForm
            defName={def.name}
            fields={def.fields}
            setup={def.setup}
            initialName={d.name}
            settings={d.settings}
            secretsSet={d.secretsSet}
            saveLabel="Save"
            onSave={async (name, values) => {
              await api.updateDestination(d.id, { name, values });
              setEditing(false);
              onChanged();
            }}
          />
          <div className="danger-zone">
            <p>Removing it stops deliveries. What it already received stays there.</p>
            <button
              className="btn btn-danger"
              onClick={async () => {
                if (!window.confirm(`Remove ${d.name}?`)) return;
                await api.deleteDestination(d.id);
                setEditing(false);
                onChanged();
              }}
            >
              Remove {d.name}
            </button>
          </div>
        </Modal>
      )}
    </article>
  );
}

function MappingRow({ d, s, route, accounts, onChanged }: { d: Destination; s: Source; route?: Route; accounts: DestAccount[] | null; onChanged: () => void }) {
  const since = route && !route.since.startsWith("0001-") ? route.since.slice(0, 10) : "";
  const save = async (account: string, sinceDate: string) => {
    if (!account) {
      if (route) await api.deleteRoute({ destinationId: d.id, sourceId: s.id });
    } else {
      await api.putRoute({ destinationId: d.id, sourceId: s.id, account, since: sinceDate ? `${sinceDate}T00:00:00Z` : "0001-01-01T00:00:00Z" });
    }
    onChanged();
  };
  return (
    <tr>
      <td>
        <strong>{s.name}</strong>
      </td>
      <td>
        <select value={route?.account ?? ""} onChange={(e) => save(e.target.value, since)} disabled={!accounts} aria-label={`Account for ${s.name}`}>
          <option value="">Not delivered</option>
          {accounts?.map((a) => (
            <option key={a.id} value={a.id}>
              {a.name}
              {a.currency ? ` (${a.currency})` : ""}
            </option>
          ))}
        </select>
      </td>
      <td>
        <input type="date" value={since} disabled={!route} onChange={(e) => route && save(route.account, e.target.value)} aria-label={`Deliver ${s.name} from`} />
      </td>
      <td className="num">
        {route && (
          <button
            className="btn btn-small btn-quiet"
            title="Forget what was delivered and send the whole history again"
            onClick={async () => {
              if (!window.confirm(`Send all of ${s.name} to ${d.name} again? Anything already there will be duplicated unless you deleted it first.`)) return;
              await api.resetRoute({ destinationId: d.id, sourceId: s.id });
              await api.push(d.id).catch(() => {});
              onChanged();
            }}
          >
            Resend all
          </button>
        )}
      </td>
    </tr>
  );
}
