// Typed calls to Fundarr's API.

export interface Field {
  name: string;
  label: string;
  secret?: boolean;
  optional?: boolean;
  help?: string;
  lines?: boolean; // a list, one value per line
  advanced?: boolean; // tucked under "Advanced"
  hidden?: boolean; // set by the UI, not a field
}

/** One saved value of a list (masked), and what it is. */
export interface ListItem {
  value: string; // masked when saved
  label?: string; // the name given to it
  kind?: string; // chain, for icons
  chain?: string;
}

/** What a value pasted in a list is, before saving. */
export interface Detected {
  kind: string;
  chain: string;
  value: string; // how to store it
  choices?: { kind: string; chain: string; value: string }[]; // other chains it can be read on
}

export interface Balance {
  account: string;
  asset: Asset;
  qty: string;
}

export interface SourceDef {
  kind: string;
  name: string;
  category: string;
  description: string;
  fields: Field[];
  setup?: string;
  statement?: string; // how to export the platform's statement (import supported)
  limits?: string; // what the platform's API cannot tell
  hidden?: boolean; // kept for existing sources, not offered for new ones
}

export interface DestDef {
  kind: string;
  name: string;
  description: string;
  fields: Field[];
  setup?: string;
}

export interface Asset {
  kind: "crypto" | "fiat" | "security";
  symbol: string;
  isin?: string;
  name?: string;
}

export interface Amount {
  asset: Asset;
  qty: string;
}

export interface Gap {
  asset: Asset;
  replayed: string;
  reported: string;
  diff: string;
}

export interface Source {
  id: string;
  kind: string;
  name: string;
  settings: Record<string, string>;
  enabled: boolean;
  created: string;
  lastRun: string;
  lastOk: string;
  lastError: string;
  category: string;
  intervalMinutes: number;
  running: boolean;
  activities: number;
  balancesAt: string;
  gaps: Gap[] | null;
  secretsSet: string[] | null;
  secretItems?: Record<string, ListItem[]>; // secret lists, masked item by item
  corrections: number; // balance corrections in the history
  adjust: boolean; // whether Fundarr makes them
}

export interface Destination {
  id: string;
  kind: string;
  name: string;
  settings: Record<string, string>;
  enabled: boolean;
  lastError: string;
  running: boolean;
  counts: Record<string, number>;
  secretsSet: string[] | null;
}

export interface Route {
  destinationId: string;
  sourceId: string;
  account: string;
  since: string;
}

export interface DestAccount {
  id: string;
  name: string;
  currency?: string;
}

export interface Activity {
  id: string;
  nativeId: string;
  time: string;
  sourceId?: string;
  type: string;
  side?: "buy" | "sell" | "in" | "out";
  base: Amount;
  quote?: Amount;
  fee?: Amount;
  network?: string;
  txId?: string;
  note?: string;
}

export interface HoldingLine {
  asset: Asset;
  replayed: string;
  correction: string;
  reported: string;
  gap: boolean;
}

export interface Adjustment extends Activity {
  created: string;
  delivered: boolean;
  releaseAt: string;
  value?: string;
  currency: string;
}

export interface Reclassify {
  as: "reward" | "deposit" | "buy" | "withdrawal" | "sell" | "fee";
  time?: string;
  quote?: { asset: { symbol: string }; qty: string };
  note?: string;
}

export interface Side extends Activity {
  source: string;
}

export interface Transfers {
  currency: string;
  sources: number;
  pairs: { by: "txid" | "amount" | "user"; out: Side; in: Side; cost?: string }[];
  unmatched: { out: Side; candidates: Side[] }[];
}

export interface AppSettings {
  currency: string;
  currencies: string[];
  prices: boolean;
}

export interface Run {
  id: number;
  kind: "sync" | "push";
  subjectId: string;
  started: string;
  ended: string;
  ok: boolean | null;
  summary: string;
  log?: string;
}

export interface Statement {
  id: number;
  name: string;
  from: string;
  to: string;
  activities: number;
  at: string;
}

export interface ImportResult {
  from: string;
  to: string;
  activities: number;
  replaced: number;
  notes: string[] | null;
  gaps: Gap[] | null;
}

export interface APIToken {
  id: string;
  name: string;
  created: string;
  lastUsed: string;
  notifyUrl: string; // told after each change, "" when not
}

export interface Session {
  user: string;
  mode: "oidc" | "password" | "proxy" | "none";
  setup: boolean; // no account yet: show the create-account screen
  local: boolean; // the request comes from a private network
  fixed: boolean; // password set by FUNDARR_PASSWORD_HASH
  auto: boolean; // go to the identity provider without a click
  version: string;
  demo: boolean; // FUNDARR_DEMO: made-up data, nothing syncs
  url: string; // FUNDARR_URL, "" when unset
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: body !== undefined ? { "Content-Type": "application/json" } : undefined,
    body: body !== undefined ? JSON.stringify(body) : undefined,
    credentials: "same-origin",
  });
  const text = await res.text();
  const data = text ? JSON.parse(text) : null;
  if (!res.ok) {
    if (res.status === 401 && path !== "/api/login" && path !== "/api/setup") window.dispatchEvent(new Event("fundarr:signedout"));
    throw new ApiError(res.status, data?.error ?? res.statusText);
  }
  return data as T;
}

export interface EntityInput {
  kind?: string;
  name?: string;
  values?: Record<string, string>;
  enabled?: boolean;
  intervalMinutes?: number;
  skipTest?: boolean;
  adjust?: boolean;
}

export const api = {
  session: () => call<Session>("GET", "/api/session"),
  login: (username: string, password: string) => call<{ user: string }>("POST", "/api/login", { username, password }),
  setup: (username: string, password: string) => call<{ user: string }>("POST", "/api/setup", { username, password }),
  changePassword: (current: string, next: string) => call("POST", "/api/password", { current, new: next }),
  logout: () => call<{ redirect: string }>("POST", "/api/logout", {}),
  definitions: () => call<{ sources: SourceDef[]; destinations: DestDef[] }>("GET", "/api/definitions"),
  sources: () => call<Source[]>("GET", "/api/sources"),
  createSource: (in_: EntityInput) => call<Source>("POST", "/api/sources", in_),
  preview: (kind: string, value: string, sourceId?: string) =>
    call<{ balances: Balance[] }>("POST", `/api/kinds/${kind}/preview`, { value, sourceId }),
  tokens: () => call<APIToken[]>("GET", "/api/tokens"),
  createToken: (name: string) => call<{ token: string; info: APIToken }>("POST", "/api/tokens", { name }),
  deleteToken: (id: string) => call<{ ok: boolean }>("DELETE", `/api/tokens/${id}`, {}),
  setNotify: (id: string, url: string) => call<{ secret: string }>("PUT", `/api/tokens/${id}/notify`, { url }),
  detect: (kind: string, value: string) => call<Detected>("POST", `/api/kinds/${kind}/detect`, { value }),
  testSource: (in_: EntityInput & { id?: string }) => call<{ ok: boolean }>("POST", "/api/sources/test", in_),
  updateSource: (id: string, in_: EntityInput) => call<Source>("PUT", `/api/sources/${id}`, in_),
  deleteSource: (id: string) => call("DELETE", `/api/sources/${id}`, {}),
  sync: (id: string, full = false) => call("POST", `/api/sources/${id}/sync`, { full }),
  activities: (id: string, offset: number, limit = 50) =>
    call<{ activities: Activity[]; total: number }>("GET", `/api/sources/${id}/activities?offset=${offset}&limit=${limit}`),
  statements: (id: string) => call<Statement[]>("GET", `/api/sources/${id}/statements`),
  importStatement: (id: string, name: string, content: string) => call<ImportResult>("POST", `/api/sources/${id}/import`, { name, content }),
  holdings: (id: string) => call<{ holdings: HoldingLine[]; balancesAt: string }>("GET", `/api/sources/${id}/holdings`),
  adjustments: (id: string) => call<Adjustment[]>("GET", `/api/sources/${id}/adjustments`),
  reclassify: (id: string, aid: string, r: Reclassify) => call<Activity>("POST", `/api/sources/${id}/adjustments/${aid}`, r),
  deleteManual: (id: string, aid: string) => call("DELETE", `/api/sources/${id}/activities/${aid}`, {}),
  transfers: () => call<Transfers>("GET", "/api/transfers"),
  putTransfer: (outId: string, inId: string, kind: "pair" | "refuse" | "") => call("PUT", "/api/transfers", { outId, inId, kind }),
  settings: () => call<AppSettings>("GET", "/api/settings"),
  putSettings: (s: { currency: string }) => call<AppSettings>("PUT", "/api/settings", s),
  destinations: () => call<{ destinations: Destination[]; routes: Route[] }>("GET", "/api/destinations"),
  createDestination: (in_: EntityInput) => call<Destination>("POST", "/api/destinations", in_),
  updateDestination: (id: string, in_: EntityInput) => call<Destination>("PUT", `/api/destinations/${id}`, in_),
  deleteDestination: (id: string) => call("DELETE", `/api/destinations/${id}`, {}),
  accounts: (id: string) => call<DestAccount[]>("GET", `/api/destinations/${id}/accounts`),
  push: (id: string) => call("POST", `/api/destinations/${id}/push`, {}),
  putRoute: (r: Partial<Route>) => call<Route>("PUT", "/api/routes", r),
  deleteRoute: (r: Partial<Route>) => call("DELETE", "/api/routes", r),
  resetRoute: (r: Partial<Route>) => call("POST", "/api/routes/reset", r),
  runs: (subject = "", limit = 50, log = false) =>
    call<Run[]>("GET", `/api/runs?subject=${encodeURIComponent(subject)}&limit=${limit}${log ? "&log=1" : ""}`),
};
