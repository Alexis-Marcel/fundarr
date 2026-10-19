import { useEffect, useState, type FormEvent } from "react";
import { Navigate, NavLink, Route, Routes } from "react-router-dom";
import { api, type DestDef, type Session, type SourceDef } from "./api";
import { ActivityPage } from "./pages/Activity";
import { DestinationsPage } from "./pages/Destinations";
import { SourceDetail } from "./pages/SourceDetail";
import { SourcesPage } from "./pages/Sources";

export function App() {
  const [session, setSession] = useState<Session | null>(null);
  const [failed, setFailed] = useState(false);
  const load = () => api.session().then(setSession, () => setFailed(true));
  useEffect(() => {
    load();
    const out = () => setSession((s) => (s ? { ...s, user: "" } : s));
    window.addEventListener("fundarr:signedout", out);
    return () => window.removeEventListener("fundarr:signedout", out);
  }, []);

  if (failed) return <div className="center"><p>Fundarr is not reachable.</p></div>;
  if (!session) return null;
  if (!session.user) return <Login session={session} onIn={load} />;
  return <Shell session={session} />;
}

function Login({ session, onIn }: { session: Session; onIn: () => void }) {
  const [pw, setPw] = useState("");
  const [err, setErr] = useState(() => new URLSearchParams(window.location.search).get("error"));
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    try {
      await api.login(pw);
      onIn();
    } catch (x) {
      setErr((x as Error).message);
    }
  };
  return (
    <div className="center">
      <div className="login">
        <Wordmark />
        <p className="muted">Your exchanges, brokers and wallets, synced into the tools you use.</p>
        {err && <p className="notice notice-error">{err}</p>}
        {session.mode === "oidc" ? (
          <a className="btn btn-primary btn-block" href="/auth/login">
            Sign in
          </a>
        ) : (
          <form onSubmit={submit} className="login-form">
            <label className="field">
              <span>Password</span>
              <input type="password" value={pw} onChange={(e) => setPw(e.target.value)} autoFocus autoComplete="current-password" />
            </label>
            <button className="btn btn-primary btn-block">Sign in</button>
          </form>
        )}
      </div>
    </div>
  );
}

function Wordmark() {
  return (
    <span className="wordmark">
      <svg viewBox="0 0 64 64" aria-hidden width="30" height="30">
        <path d="M16 46V18h30M16 32h22" stroke="currentColor" strokeWidth="7" strokeLinecap="round" strokeLinejoin="round" fill="none" />
        <circle cx="46" cy="46" r="5" className="wordmark-dot" />
      </svg>
      Fundarr
    </span>
  );
}

function Shell({ session }: { session: Session }) {
  const [defs, setDefs] = useState<{ sources: SourceDef[]; destinations: DestDef[] } | null>(null);
  const [names, setNames] = useState<Record<string, string>>({});
  useEffect(() => {
    api.definitions().then(setDefs);
    Promise.all([api.sources(), api.destinations()]).then(([s, d]) => {
      const n: Record<string, string> = {};
      s.forEach((x) => (n[x.id] = x.name));
      d.destinations.forEach((x) => (n[x.id] = x.name));
      setNames(n);
    });
  }, []);

  return (
    <div className="shell">
      <aside className="nav">
        <Wordmark />
        <nav>
          <NavLink to="/sources">Sources</NavLink>
          <NavLink to="/destinations">Destinations</NavLink>
          <NavLink to="/activity">Activity log</NavLink>
        </nav>
        <div className="nav-foot">
          <span className="muted small">{session.user}</span>
          {session.mode !== "none" && (
            <button
              className="link small"
              onClick={async () => {
                const r = await api.logout();
                window.location.href = r.redirect;
              }}
            >
              Sign out
            </button>
          )}
          <span className="muted small version">v{session.version}</span>
        </div>
      </aside>
      <main className="main">
        {defs && (
          <Routes>
            <Route path="/" element={<Navigate to="/sources" replace />} />
            <Route path="/sources" element={<SourcesPage defs={defs.sources} />} />
            <Route path="/sources/:id" element={<SourceDetail defs={defs.sources} />} />
            <Route path="/destinations" element={<DestinationsPage defs={defs.destinations} />} />
            <Route path="/activity" element={<ActivityPage names={names} />} />
            <Route path="*" element={<Navigate to="/sources" replace />} />
          </Routes>
        )}
      </main>
    </div>
  );
}
