import { useState, type FormEvent } from "react";
import type { Field } from "../api";

interface Props {
  defName: string;
  fields: Field[];
  setup?: string;
  initialName?: string;
  settings?: Record<string, string>;
  secretsSet?: string[] | null;
  /** Checks the values against the service without saving. */
  onTest?: (name: string, values: Record<string, string>) => Promise<void>;
  onSave: (name: string, values: Record<string, string>) => Promise<void>;
  saveLabel: string;
  extra?: React.ReactNode;
}

/** The settings form of a source or destination, built from its fields. */
export function EntityForm(p: Props) {
  const [name, setName] = useState(p.initialName ?? p.defName);
  const [values, setValues] = useState<Record<string, string>>(() => ({ ...(p.settings ?? {}) }));
  const [busy, setBusy] = useState<"" | "test" | "save">("");
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const saved = new Set(p.secretsSet ?? []);

  const run = async (kind: "test" | "save") => {
    setBusy(kind);
    setMsg(null);
    try {
      if (kind === "test") {
        await p.onTest!(name, values);
        setMsg({ ok: true, text: "Connection works. The key is read-only." });
      } else {
        await p.onSave(name, values);
      }
    } catch (e) {
      setMsg({ ok: false, text: (e as Error).message });
    } finally {
      setBusy("");
    }
  };

  return (
    <form
      className="entity-form"
      onSubmit={(e: FormEvent) => {
        e.preventDefault();
        run("save");
      }}
    >
      {p.setup && <p className="setup">{p.setup}</p>}
      <label className="field">
        <span>Name</span>
        <input value={name} onChange={(e) => setName(e.target.value)} maxLength={60} />
      </label>
      {p.fields.map((f) => (
        <label className="field" key={f.name}>
          <span>
            {f.label}
            {f.optional && <em className="opt"> optional</em>}
          </span>
          {f.lines ? (
            <textarea
              rows={Math.max(3, (values[f.name] ?? "").split("\n").length + 1)}
              autoComplete="off"
              spellCheck={false}
              value={values[f.name] ?? ""}
              placeholder="One per line"
              onChange={(e) => setValues((v) => ({ ...v, [f.name]: e.target.value }))}
              required={!f.optional && !(f.secret && saved.has(f.name))}
            />
          ) : (
            <input
              type={f.secret ? "password" : "text"}
              autoComplete="off"
              spellCheck={false}
              value={values[f.name] ?? ""}
              placeholder={f.secret && saved.has(f.name) ? "Saved. Leave empty to keep it" : ""}
              onChange={(e) => setValues((v) => ({ ...v, [f.name]: e.target.value }))}
              required={!f.optional && !(f.secret && saved.has(f.name))}
            />
          )}
          {f.help && <small>{f.help}</small>}
          {f.lines && f.secret && saved.has(f.name) && (
            <small>Saved lines show hidden in the middle; keep them as they are, remove the ones you no longer want, add new ones.</small>
          )}
        </label>
      ))}
      {p.extra}
      {msg && (
        <p className={msg.ok ? "notice notice-ok" : "notice notice-error"} role="status">
          {msg.text}
        </p>
      )}
      <div className="form-actions">
        {p.onTest && (
          <button type="button" className="btn" disabled={busy !== ""} onClick={() => run("test")}>
            {busy === "test" ? "Testing…" : "Test connection"}
          </button>
        )}
        <button className="btn btn-primary" disabled={busy !== ""}>
          {busy === "save" ? "Saving…" : p.saveLabel}
        </button>
      </div>
    </form>
  );
}
