import { useEffect, useState } from "react";
import { api, type DestAccount, type DestDef, type Destination, type Route, type Source } from "../api";
import { KindBadge, StatusDot } from "../components/Badge";
import { EntityForm } from "../components/EntityForm";
import { Modal } from "../components/Modal";
import { usePoll } from "../lib";

export function DestinationsPage({ defs }: { defs: DestDef[] }) {
  const { data, reload } = usePoll(async () => {
    const [d, s] = await Promise.all([api.destinations(), api.sources()]);
    return { ...d, sources: s };
  }, 8000);
  const [adding, setAdding] = useState(false);

  return (
    <section className="page">
      <header className="page-head">
        <div>
          <h1>Destinations</h1>
          <p className="lede">Where the history goes. Map each source to an account; new activities are delivered after every sync.</p>
        </div>
        <button className="btn btn-primary" onClick={() => setAdding(true)}>
          Add destination
        </button>
      </header>
      {data && data.destinations.length === 0 && (
        <div className="empty">
          <h2>No destination yet</h2>
          <p>Connect a portfolio tracker such as Wealthfolio. Fundarr imports each source's history into the account you choose.</p>
          <button className="btn btn-primary" onClick={() => setAdding(true)}>
            Add a destination
          </button>
        </div>
      )}
      {data?.destinations.map((d) => (
        <DestinationCard key={d.id} d={d} def={defs.find((x) => x.kind === d.kind)} routes={data.routes.filter((r) => r.destinationId === d.id)} sources={data.sources} onChanged={reload} />
      ))}
      {adding && (
        <Modal title="Add a destination" onClose={() => setAdding(false)}>
          <AddDestination defs={defs} onDone={() => { setAdding(false); reload(); }} />
        </Modal>
      )}
    </section>
  );
}

function AddDestination({ defs, onDone }: { defs: DestDef[]; onDone: () => void }) {
  const [def, setDef] = useState<DestDef | null>(defs.length === 1 ? defs[0] : null);
  if (!def)
    return (
      <div className="picker">
        {defs.map((d) => (
          <button key={d.kind} className="pick" onClick={() => setDef(d)}>
            <KindBadge kind={d.kind} size={44} />
            <span>
              <strong>{d.name}</strong>
              <small>{d.description}</small>
            </span>
          </button>
        ))}
      </div>
    );
  return (
    <EntityForm
      defName={def.name}
      fields={def.fields}
      setup={def.setup}
      saveLabel="Connect"
      onSave={async (name, values) => {
        await api.createDestination({ kind: def.kind, name, values });
        onDone();
      }}
    />
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
            <p>Removing the destination stops deliveries. What it already received stays there.</p>
            <button
              className="btn btn-danger"
              onClick={async () => {
                if (!window.confirm(`Remove ${d.name}?`)) return;
                await api.deleteDestination(d.id);
                setEditing(false);
                onChanged();
              }}
            >
              Remove destination
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
