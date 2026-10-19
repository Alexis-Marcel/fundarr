import { useRef, useState, type ClipboardEvent, type KeyboardEvent } from "react";
import { api, type Detected, type ListItem } from "../api";

interface Item extends ListItem {
  choices?: Detected["choices"];
}

interface Props {
  kind: string;
  label: string;
  help?: string;
  /** Saved items, masked, with what each one is. */
  saved?: ListItem[];
  onChange: (value: string) => void;
}

/** A list of keys and addresses: each one recognized before it is added. */
export function AccountList(p: Props) {
  const [items, setItems] = useState<Item[]>(() => p.saved ?? []);
  const [draft, setDraft] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const input = useRef<HTMLInputElement>(null);

  // Changes go through the latest list: several adds may be in flight.
  const update = (change: (cur: Item[]) => Item[]) =>
    setItems((cur) => {
      const next = change(cur);
      p.onChange(next.map((i) => i.value).join("\n"));
      return next;
    });

  // The field's own value is the truth: a paste may land before the state.
  const add = async (text?: string) => {
    const v = (text ?? input.current?.value ?? draft).trim();
    if (!v || busy) return;
    setBusy(true);
    setError("");
    try {
      const d = await api.detect(p.kind, v);
      const same = (i: Item) => i.value === d.value || !!d.choices?.some((c) => c.value === i.value);
      if (items.some(same)) {
        setError("This account is already in the list.");
      } else {
        update((cur) => (cur.some(same) ? cur : [...cur, { value: d.value, chain: d.chain, choices: d.choices }]));
        setDraft("");
      }
    } catch (e) {
      const m = (e as Error).message;
      setError(m.charAt(0).toUpperCase() + m.slice(1));
    } finally {
      setBusy(false);
    }
  };

  // Pasting a key adds it at once: there is nothing else to type.
  const onPaste = (e: ClipboardEvent<HTMLInputElement>) => {
    const text = e.clipboardData.getData("text");
    if (text.trim() && !draft.trim()) {
      e.preventDefault();
      setDraft(text.trim());
      add(text);
    }
  };

  const onKey = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Enter") {
      e.preventDefault();
      add();
    }
  };

  return (
    <div className="field">
      <span>{p.label}</span>
      {items.length > 0 && (
        <ul className="accounts">
          {items.map((it, i) => (
            <li key={it.value}>
              {it.choices && it.choices.length > 1 ? (
                <select
                  className="account-chain"
                  value={it.value}
                  aria-label="Read this key as"
                  onChange={(e) => {
                    const c = it.choices!.find((c) => c.value === e.target.value)!;
                    update((cur) => cur.map((x, j) => (j === i ? { ...x, value: c.value, chain: c.chain } : x)));
                  }}
                >
                  {it.choices.map((c) => (
                    <option key={c.value} value={c.value}>
                      {c.chain}
                    </option>
                  ))}
                </select>
              ) : (
                <strong className="account-chain">{it.chain || "Account"}</strong>
              )}
              <code className="account-key" title={it.value.includes("…") ? "Saved, shown shortened" : it.value}>
                {short(it.value)}
              </code>
              <button type="button" className="link small" onClick={() => update((cur) => cur.filter((_, j) => j !== i))}>
                Remove
              </button>
            </li>
          ))}
        </ul>
      )}
      <div className="account-add">
        <input
          ref={input}
          value={draft}
          autoComplete="off"
          spellCheck={false}
          placeholder={items.length ? "Add another key or address" : "Paste a key or address"}
          aria-label={`Add to ${p.label}`}
          onChange={(e) => {
            setDraft(e.target.value);
            setError("");
          }}
          onKeyDown={onKey}
          onPaste={onPaste}
        />
        <button type="button" className="btn" disabled={busy} onClick={() => add()}>
          {busy ? "Checking…" : "Add"}
        </button>
      </div>
      {error && <small className="field-error">{error}</small>}
      {p.help && (
        <ul className="field-help">
          {p.help.split("\n").map((l) => (
            <li key={l}>{l}</li>
          ))}
        </ul>
      )}
    </div>
  );
}

/** A long key fits on one row: its start and its end. */
function short(v: string) {
  const key = v.slice(v.indexOf(":") + 1);
  if (v.includes("…") || key.length <= 24) return v;
  return v.slice(0, v.length - key.length) + key.slice(0, 10) + "…" + key.slice(-6);
}
