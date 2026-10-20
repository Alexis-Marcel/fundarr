import { useEffect, useRef, useState } from "react";
import type { Asset } from "./api";

const rtf = new Intl.RelativeTimeFormat("en", { numeric: "auto" });

/** "3 minutes ago", "in 2 hours"; "never" for the zero time. */
export function ago(iso: string | undefined): string {
  if (!iso || iso.startsWith("0001-")) return "never";
  const s = (new Date(iso).getTime() - Date.now()) / 1000;
  const a = Math.abs(s);
  if (a < 45) return "just now";
  if (a < 3600) return rtf.format(Math.round(s / 60), "minute");
  if (a < 86400) return rtf.format(Math.round(s / 3600), "hour");
  if (a < 86400 * 30) return rtf.format(Math.round(s / 86400), "day");
  return new Date(iso).toLocaleDateString("en", { year: "numeric", month: "short", day: "numeric" });
}

export function isZeroTime(iso: string | undefined) {
  return !iso || iso.startsWith("0001-");
}

export function dateTime(iso: string) {
  return new Date(iso).toLocaleString("en", { year: "numeric", month: "short", day: "2-digit", hour: "2-digit", minute: "2-digit" });
}

/** Quantities keep their precision but lose trailing zeros and get grouping. */
export function qty(v: string): string {
  if (!v) return "0";
  const neg = v.startsWith("-");
  const [int, frac = ""] = (neg ? v.slice(1) : v).split(".");
  const grouped = int.replace(/\B(?=(\d{3})+(?!\d))/g, " ");
  const f = frac.replace(/0+$/, "").slice(0, 10);
  return (neg ? "−" : "") + grouped + (f ? "." + f : "");
}

/** How an asset is named: a security by its name (its ISIN is no name). */
export function assetLabel(a: Asset) {
  if (a.kind === "security" && a.name) return a.name;
  return a.symbol || a.isin || "?";
}

/** The sub-account of a source, for people: "" is the main one. */
export function accountName(a: string) {
  return a === "" ? "Main account" : a;
}

/** "14:32" today, "Tue 14:32" this week, a date otherwise. */
export function until(iso: string) {
  const d = new Date(iso);
  const now = new Date();
  const time = d.toLocaleTimeString("en", { hour: "2-digit", minute: "2-digit" });
  if (d.toDateString() === now.toDateString()) return time;
  if (d.getTime() - now.getTime() < 6 * 86400000) return `${d.toLocaleDateString("en", { weekday: "short" })} ${time}`;
  return `${d.toLocaleDateString("en", { month: "short", day: "numeric" })} ${time}`;
}

/** Repeats fn every `ms` while mounted (and once immediately). */
export function usePoll<T>(fn: () => Promise<T>, ms: number, deps: unknown[] = []) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const fnRef = useRef(fn);
  fnRef.current = fn;
  const [tick, setTick] = useState(0);
  useEffect(() => {
    let stop = false;
    let timer: number | undefined;
    const load = async () => {
      try {
        const d = await fnRef.current();
        if (!stop) {
          setData(d);
          setError(null);
        }
      } catch (e) {
        if (!stop) setError((e as Error).message);
      }
      if (!stop) timer = window.setTimeout(load, ms);
    };
    load();
    return () => {
      stop = true;
      window.clearTimeout(timer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ms, tick, ...deps]);
  return { data, error, reload: () => setTick((t) => t + 1), setData };
}

/** A short monogram and a stable hue for a connector kind. */
export function monogram(kind: string) {
  const known: Record<string, [string, number]> = {
    binance: ["BN", 45],
    okx: ["OK", 210],
    wealthfolio: ["WF", 150],
    ghostfolio: ["GF", 260],
    cairn: ["C", 95],
    app: ["API", 200],
    traderepublic: ["TR", 0],
    bitcoin: ["₿", 32],
    evm: ["Ξ", 230],
    wallet: ["W", 190],
    other: ["W", 190],
    ledger: ["L", 215],
    trezor: ["T", 150],
    metamask: ["M", 28],
    phantom: ["P", 265],
    trust: ["TW", 210],
    exodus: ["EX", 285],
  };
  if (known[kind]) return known[kind];
  let h = 0;
  for (const c of kind) h = (h * 31 + c.charCodeAt(0)) % 360;
  return [kind.slice(0, 2).toUpperCase(), h] as [string, number];
}

/** A value in a currency: "1 234.50 EUR". */
export function money(v: string | undefined, ccy: string): string {
  if (!v) return "";
  const n = Number(v);
  try {
    return n.toLocaleString("en", { style: "currency", currency: ccy, maximumFractionDigits: 2 });
  } catch {
    return `${n.toFixed(2)} ${ccy}`;
  }
}

export function day(iso: string) {
  return new Date(iso).toLocaleDateString("en", { year: "numeric", month: "short", day: "numeric" });
}

/** The badge of a source: a Wallet shows the app it was added for. */
export function badgeKind(s: { kind: string; settings?: Record<string, string> | null }) {
  return s.kind === "wallet" && s.settings?.app ? s.settings.app : s.kind;
}
