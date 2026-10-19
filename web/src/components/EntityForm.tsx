import { useState, type FormEvent } from "react";
import type { Field, ListItem } from "../api";
import { AccountList } from "./AccountList";

interface Props {
  /** The source kind, for lists whose values the server recognizes. */
  kind?: string;
  defName: string;
  fields: Field[];
  setup?: string;
  initialName?: string;
  settings?: Record<string, string>;
  secretsSet?: string[] | null;
  secretItems?: Record<string, ListItem[]>;
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

  const field = (f: Field) =>
        f.lines && p.kind ? (
          <AccountList
            key={f.name}
            kind={p.kind}
            label={f.label}
            help={f.help}
            saved={p.secretItems?.[f.name]}
            onChange={(v) => setValues((x) => ({ ...x, [f.name]: v }))}
          />
        ) : (
          <label className="field" key={f.name}>
            <span>
              {f.label}
              {f.optional && <em className="opt"> optional</em>}
            </span>
            <input
              type={f.secret ? "password" : "text"}
              autoComplete="off"
              spellCheck={false}
              value={values[f.name] ?? ""}
              placeholder={f.secret && saved.has(f.name) ? "Saved. Leave empty to keep it" : ""}
              onChange={(e) => setValues((v) => ({ ...v, [f.name]: e.target.value }))}
              required={!f.optional && !(f.secret && saved.has(f.name))}
            />
            {f.help && <small>{f.help}</small>}
          </label>
        );

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
      {p.fields.filter((f) => !f.advanced).map(field)}
      {p.fields.some((f) => f.advanced) && (
        <details className="advanced">
          <summary>Advanced</summary>
          {p.fields.filter((f) => f.advanced).map(field)}
        </details>
      )}
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
