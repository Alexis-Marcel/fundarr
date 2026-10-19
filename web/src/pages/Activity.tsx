import { useState } from "react";
import { api, type Run } from "../api";
import { StatusDot } from "../components/Badge";
import { ago, dateTime, usePoll } from "../lib";

export function ActivityPage({ names }: { names: Record<string, string> }) {
  return (
    <section className="page">
      <header className="page-head">
        <div>
          <h1>Activity log</h1>
          <p className="lede">Every sync and every delivery, newest first. Open one to read its log.</p>
        </div>
      </header>
      <RunList subject="" names={names} />
    </section>
  );
}

export function RunList({ subject, names }: { subject: string; names?: Record<string, string> }) {
  const { data } = usePoll(() => api.runs(subject, 100), 5000, [subject]);
  const [open, setOpen] = useState<number | null>(null);
  if (!data) return <p className="muted">Loading…</p>;
  if (data.length === 0) return <p className="muted">Nothing has run yet.</p>;
  return (
    <ul className="runs">
      {data.map((r) => (
        <RunItem key={r.id} run={r} name={names?.[r.subjectId]} open={open === r.id} onToggle={() => setOpen(open === r.id ? null : r.id)} />
      ))}
    </ul>
  );
}

function RunItem({ run, name, open, onToggle }: { run: Run; name?: string; open: boolean; onToggle: () => void }) {
  const { data } = usePoll(async () => (open ? (await api.runs(run.subjectId, 200, true)).find((x) => x.id === run.id) ?? null : null), open && run.ok === null ? 2000 : 600000, [open]);
  const h = run.ok === null ? "running" : run.ok ? "ok" : "error";
  return (
    <li className={`run ${open ? "is-open" : ""}`}>
      <button className="run-head" onClick={onToggle} aria-expanded={open}>
        <StatusDot health={h} />
        <span className="run-kind">{run.kind === "sync" ? "Sync" : "Delivery"}</span>
        {name && <span className="run-name">{name}</span>}
        <span className="run-summary clip">{run.ok === null ? "Running…" : run.summary}</span>
        <time className="muted small" dateTime={run.started} title={dateTime(run.started)}>
          {ago(run.started)}
        </time>
      </button>
      {open && <pre className="run-log">{data?.log || "No log lines."}</pre>}
    </li>
  );
}
