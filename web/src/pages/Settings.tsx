import { useState, type FormEvent } from "react";
import { api, type Session } from "../api";
import { ago, isZeroTime, usePoll } from "../lib";

const MODE: Record<Session["mode"], string> = {
  password: "Username and password",
  oidc: "Single sign-on (OpenID Connect)",
  proxy: "Reverse proxy (user from a trusted header)",
  none: "None: anyone who can reach Fundarr can use it",
};

export function SettingsPage({ session }: { session: Session }) {
  return (
    <section className="page">
      <header className="page-head">
        <div>
          <h1>Settings</h1>
          <p className="lede">How values are shown, and how you sign in to Fundarr.</p>
        </div>
      </header>
      <Valuation />
      <ExportCard />
      <div className="card settings-card">
        <dl className="facts">
          <dt>Sign-in</dt>
          <dd>{MODE[session.mode]}</dd>
          <dt>Signed in as</dt>
          <dd>{session.user}</dd>
          <dt>Version</dt>
          <dd>{session.version}</dd>
        </dl>
        {session.mode === "password" && (session.fixed ? <p className="muted small">The password is set by FUNDARR_PASSWORD_HASH; change it there.</p> : <ChangePassword />)}
        {session.mode !== "password" && (
          <p className="muted small">
            The sign-in method is chosen with environment variables: FUNDARR_OIDC_ISSUER for single sign-on, FUNDARR_AUTH=proxy or none. See the README.
          </p>
        )}
      </div>
    </section>
  );
}

function ChangePassword() {
  const [cur, setCur] = useState("");
  const [next, setNext] = useState("");
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    try {
      await api.changePassword(cur, next);
      setCur("");
      setNext("");
      setMsg({ ok: true, text: "Password changed." });
    } catch (x) {
      setMsg({ ok: false, text: (x as Error).message });
    }
  };
  return (
    <form className="entity-form narrow" onSubmit={submit}>
      <h2>Change password</h2>
      <label className="field">
        <span>Current password</span>
        <input type="password" value={cur} onChange={(e) => setCur(e.target.value)} autoComplete="current-password" required />
      </label>
      <label className="field">
        <span>New password</span>
        <input type="password" value={next} onChange={(e) => setNext(e.target.value)} autoComplete="new-password" minLength={8} required />
        <small>At least 8 characters.</small>
      </label>
      {msg && <p className={msg.ok ? "notice notice-ok" : "notice notice-error"}>{msg.text}</p>}
      <div className="form-actions">
        <button className="btn btn-primary">Change password</button>
      </div>
    </form>
  );
}

function Valuation() {
  const { data, setData } = usePoll(() => api.settings(), 600000);
  if (!data) return null;
  return (
    <div className="card settings-card">
      <h2>Valuation</h2>
      <label className="field narrow">
        <span>Currency</span>
        <select value={data.currency} onChange={async (e) => setData(await api.putSettings({ currency: e.target.value }))}>
          {data.currencies.map((c) => (
            <option key={c}>{c}</option>
          ))}
        </select>
        <small>
          Corrections and transfers are valued in it, with daily prices from Binance and OKX market data and the European Central Bank&apos;s
          exchange rates. Destinations get costs in each account&apos;s own currency.
        </small>
      </label>
    </div>
  );
}

/** Downloads, and tokens for programs that read Fundarr (the /api/v1 routes). */
function ExportCard() {
  const { data, reload } = usePoll(() => api.tokens(), 600000);
  const [name, setName] = useState("");
  const [made, setMade] = useState<string | null>(null);
  const create = async (e: FormEvent) => {
    e.preventDefault();
    try {
      const r = await api.createToken(name);
      setMade(r.token);
      setName("");
      reload();
    } catch (err) {
      window.alert((err as Error).message);
    }
  };
  return (
    <div className="card settings-card">
      <h2>Export and API</h2>
      <div className="form-actions start">
        <a className="btn" href="/api/v1/activities.csv?format=fundarr" download>
          Download CSV
        </a>
        <a className="btn" href="/api/v1/activities.csv?format=koinly" download>
          Koinly CSV
        </a>
      </div>
      <p className="muted small">
        Other apps read <code>/api/v1/activities</code> with a token: <code>Authorization: Bearer …</code>
      </p>
      {made && (
        <p className="notice notice-ok">
          Copy it now, it won&apos;t show again: <code className="select">{made}</code>
        </p>
      )}
      {data && data.length > 0 && (
        <ul className="token-list">
          {data.map((t) => (
            <li key={t.id}>
              <strong>{t.name}</strong>
              <span className="muted small">{isZeroTime(t.lastUsed) ? "never used" : `used ${ago(t.lastUsed)}`}</span>
              <button
                className="link small"
                onClick={async () => {
                  if (!window.confirm(`Revoke "${t.name}"?`)) return;
                  await api.deleteToken(t.id);
                  reload();
                }}
              >
                Revoke
              </button>
            </li>
          ))}
        </ul>
      )}
      <form className="account-add" onSubmit={create}>
        <input value={name} onChange={(e) => setName(e.target.value)} placeholder="Token name (what will use it)" maxLength={60} />
        <button className="btn" disabled={!name.trim()}>
          New token
        </button>
      </form>
    </div>
  );
}
