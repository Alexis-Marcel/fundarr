import { useEffect, useState, type FormEvent } from "react";
import { Navigate, NavLink, Route, Routes } from "react-router-dom";
import { api, type DestDef, type Session, type SourceDef } from "./api";
import { ActivityPage } from "./pages/Activity";
import { AppsPage } from "./pages/Apps";
import { SettingsPage } from "./pages/Settings";
import { TransfersPage } from "./pages/Transfers";
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
  const [user, setUser] = useState(session.setup ? "admin" : "");
  const [pw, setPw] = useState("");
  const [pw2, setPw2] = useState("");
  const [err, setErr] = useState(() => new URLSearchParams(window.location.search).get("error"));
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setErr(null);
    if (session.setup && pw !== pw2) {
      setErr("The two passwords differ.");
      return;
    }
    try {
      if (session.setup) await api.setup(user, pw);
      else await api.login(user, pw);
      window.history.replaceState(null, "", "/");
      onIn();
    } catch (x) {
      setErr((x as Error).message);
    }
  };

  // Single sign-on: go to the provider at once. With a session there, the
  // user is back signed in without a click. Not after an error, and at
  // most once every 30 s, so a failing sign-in cannot loop.
  const autoGo = session.mode === "oidc" && session.auto && !err && recentAttempt() === false;
  useEffect(() => {
    if (!autoGo) return;
    try {
      sessionStorage.setItem("fundarr:sso", String(Date.now()));
    } catch {
      /* private mode: no guard, still fine once */
    }
    window.location.replace("/auth/login");
  }, [autoGo]);
  if (autoGo) return null;

  if (session.mode === "proxy")
    return (
      <div className="center">
        <div className="login">
          <Wordmark />
          <p className="notice notice-error">The proxy in front of Fundarr did not say who you are. Sign in through it, or check its Remote-User header.</p>
        </div>
      </div>
    );

  return (
    <div className="center">
      <div className="login">
        <Wordmark />
        {session.setup ? (
          <>
            <h1 className="login-title">Create your account</h1>
            <p className="muted">Fundarr will hold read-only keys to your exchanges. Choose the username and password that protect them.</p>
          </>
        ) : (
          <p className="muted">Your exchanges, brokers and wallets, synced into the tools you use.</p>
        )}
        {err && <p className="notice notice-error">{err}</p>}
        {session.mode === "oidc" ? (
          <a className="btn btn-primary btn-block" href="/auth/login">
            Sign in
          </a>
        ) : session.setup && !session.local ? (
          <p className="notice notice-error">
            For safety, the account can only be created from your local network or VPN. Open Fundarr from there, or set FUNDARR_PASSWORD_HASH.
          </p>
        ) : (
          <form onSubmit={submit} className="login-form">
            <label className="field">
              <span>Username</span>
              <input value={user} onChange={(e) => setUser(e.target.value)} autoFocus={!session.setup} autoComplete="username" required />
            </label>
            <label className="field">
              <span>Password</span>
              <input
                type="password"
                value={pw}
                onChange={(e) => setPw(e.target.value)}
                autoFocus={session.setup}
                autoComplete={session.setup ? "new-password" : "current-password"}
                minLength={session.setup ? 8 : undefined}
                required
              />
              {session.setup && <small>At least 8 characters.</small>}
            </label>
            {session.setup && (
              <label className="field">
                <span>Password again</span>
                <input type="password" value={pw2} onChange={(e) => setPw2(e.target.value)} autoComplete="new-password" required />
              </label>
            )}
            <button className="btn btn-primary btn-block">{session.setup ? "Create account" : "Sign in"}</button>
          </form>
        )}
      </div>
    </div>
  );
}

function recentAttempt(): boolean {
  try {
    const t = Number(sessionStorage.getItem("fundarr:sso") ?? 0);
    return Date.now() - t < 30_000;
  } catch {
    return false;
  }
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
          <NavLink to="/apps">Apps</NavLink>
          <NavLink to="/transfers">Transfers</NavLink>
          <NavLink to="/activity">Activity log</NavLink>
          <NavLink to="/settings">Settings</NavLink>
        </nav>
        <div className="nav-foot">
          <span className="muted small">{session.user}</span>
          {(session.mode === "password" || session.mode === "oidc") && (
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
        {session.demo && (
          <p className="demo-banner">
            <strong>Demo data</strong> Made up, nothing syncs.
          </p>
        )}
        {defs && (
          <Routes>
            <Route path="/" element={<Navigate to="/sources" replace />} />
            <Route path="/sources" element={<SourcesPage defs={defs.sources} />} />
            <Route path="/sources/:id" element={<SourceDetail defs={defs.sources} />} />
            <Route path="/apps" element={<AppsPage defs={defs.destinations} session={session} />} />
            <Route path="/destinations" element={<Navigate to="/apps" replace />} />
            <Route path="/transfers" element={<TransfersPage />} />
            <Route path="/activity" element={<ActivityPage names={names} />} />
            <Route path="/settings" element={<SettingsPage session={session} />} />
            <Route path="*" element={<Navigate to="/sources" replace />} />
          </Routes>
        )}
      </main>
    </div>
  );
}
