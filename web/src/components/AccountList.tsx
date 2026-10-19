import { useEffect, useRef, useState, type ClipboardEvent, type KeyboardEvent, type ReactNode } from "react";
import { api, type Balance, type Detected, type ListItem } from "../api";
import { assetLabel, qty } from "../lib";
import { CryptoIcon } from "./CryptoIcon";

interface Item extends ListItem {
  choices?: Detected["choices"];
}

/** What reading an account found: its balances, or why it could not. */
type Look = { state: "checking" } | { state: "done"; balances: Balance[] } | { state: "error"; message: string };

interface Props {
  kind: string;
  label: string;
  /** The saved source, to read its saved (masked) items. */
  sourceId?: string;
  /** Saved items, masked, with what each one is. */
  saved?: ListItem[];
  /** How to find what to paste, shown under the field. */
  guide?: ReactNode;
  onChange: (value: string) => void;
}

const line = (i: Item) => (i.label ? `${i.value}\t${i.label}` : i.value);

/** A list of keys and addresses: each one recognized, named and read before it is saved. */
export function AccountList(p: Props) {
  const [items, setItems] = useState<Item[]>(() => p.saved ?? []);
  const [looks, setLooks] = useState<Record<string, Look>>({});
  const [draft, setDraft] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const input = useRef<HTMLInputElement>(null);

  // Changes go through the latest list: several adds may be in flight.
  const update = (change: (cur: Item[]) => Item[]) =>
    setItems((cur) => {
      const next = change(cur);
      p.onChange(next.map(line).join("\n"));
      return next;
    });

  const look = (value: string) => {
    setLooks((l) => ({ ...l, [value]: { state: "checking" } }));
    api
      .preview(p.kind, value, value.includes("…") ? p.sourceId : undefined)
      .then((r) => setLooks((l) => ({ ...l, [value]: { state: "done", balances: r.balances } })))
      .catch((e) => setLooks((l) => ({ ...l, [value]: { state: "error", message: (e as Error).message } })));
  };

  // Saved accounts are read once, when the form opens.
  useEffect(() => {
    (p.saved ?? []).forEach((i) => look(i.value));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

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
        update((cur) => (cur.some(same) ? cur : [...cur, { value: d.value, kind: d.kind, chain: d.chain, choices: d.choices }]));
        setDraft("");
        look(d.value);
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
              <CryptoIcon kind={it.kind} size={28} />
              <div className="account-main">
                <div className="account-head">
                  <input
                    className="account-name"
                    value={it.label ?? ""}
                    placeholder={it.chain || "Account"}
                    aria-label="Name of this account"
                    maxLength={40}
                    onChange={(e) => {
                      const label = e.target.value.replace(/\t/g, " ");
                      update((cur) => cur.map((x, j) => (j === i ? { ...x, label } : x)));
                    }}
                  />
                </div>
                <div className="account-sub">
                  <code title={it.value.includes("…") ? "Saved, shown shortened" : it.value}>{short(it.value)}</code>
                  <Found
                    look={looks[it.value]}
                    others={it.choices?.filter((c) => c.value !== it.value)}
                    onReadAs={(c) => {
                      update((cur) => cur.map((x, j) => (j === i ? { ...x, value: c.value, kind: c.kind, chain: c.chain } : x)));
                      look(c.value);
                    }}
                  />
                </div>
              </div>
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
          placeholder="Paste a key or address"
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
      {p.guide}
    </div>
  );
}

type Choice = NonNullable<Detected["choices"]>[number];

/** What an account holds now, in one line. An empty one whose key other
 * chains share (a plain xpub) offers to read it as those. */
function Found({ look, others, onReadAs }: { look?: Look; others?: Choice[]; onReadAs: (c: Choice) => void }) {
  if (!look) return null;
  if (look.state === "checking") return <span className="found muted">Reading…</span>;
  if (look.state === "done" && look.balances.length === 0 && others && others.length > 0) {
    return (
      <span className="found found-warn">
        Empty. Read it as{" "}
        {others.map((c, k) => (
          <span key={c.value}>
            {k > 0 && (k === others.length - 1 ? " or " : ", ")}
            <button type="button" className="link" onClick={() => onReadAs(c)}>
              {c.chain}
            </button>
          </span>
        ))}
      </span>
    );
  }
  if (look.state === "error") {
    return <span className="found found-error">{look.message.charAt(0).toUpperCase() + look.message.slice(1)}</span>;
  }
  if (look.balances.length === 0) {
    return <span className="found found-warn">Empty. Right account?</span>;
  }
  // One amount per asset, networks summed.
  const by = new Map<string, { b: Balance; q: number }>();
  for (const b of look.balances) {
    const cur = by.get(b.asset.symbol) ?? { b, q: 0 };
    cur.q += Number(b.qty);
    by.set(b.asset.symbol, cur);
  }
  const all = [...by.values()];
  const nets = new Set(look.balances.map((b) => b.account).filter(Boolean));
  return (
    <span className="found">
      {all
        .slice(0, 3)
        .map((x) => `${qty(x.q.toFixed(8))} ${assetLabel(x.b.asset)}`)
        .join(" · ")}
      {all.length > 3 && ` · +${all.length - 3} more`}
      {nets.size > 1 && <span className="muted"> on {[...nets].join(", ")}</span>}
    </span>
  );
}

/** A long key fits on one row: its start and its end. */
function short(v: string) {
  const key = v.slice(v.indexOf(":") + 1);
  if (v.includes("…") || key.length <= 24) return v;
  return v.slice(0, v.length - key.length) + key.slice(0, 10) + "…" + key.slice(-6);
}
