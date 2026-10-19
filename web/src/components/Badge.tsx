import { monogram } from "../lib";

export function KindBadge({ kind, size = 40 }: { kind: string; size?: number }) {
  const [txt, hue] = monogram(kind);
  return (
    <span className="kind-badge" style={{ width: size, height: size, ["--hue" as string]: hue }} aria-hidden>
      {txt}
    </span>
  );
}

export type Health = "ok" | "warn" | "error" | "idle" | "running";

export function StatusDot({ health }: { health: Health }) {
  return <span className={`dot dot-${health}`} aria-hidden />;
}
