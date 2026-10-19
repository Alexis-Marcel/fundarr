import type { WalletApp } from "../wallets";
import { CryptoIcon } from "./CryptoIcon";

/** Where the chosen wallet app shows what Fundarr needs, chain by chain. */
export function WalletGuide({ app }: { app: WalletApp }) {
  return (
    <div className="guide">
      <p>{app.intro}</p>
      <ol className="guide-steps">
        {app.steps.map((s) => (
          <li key={s.title}>
            <span className="guide-icons" aria-hidden>
              {s.kinds.map((k) => (
                <CryptoIcon key={k} kind={k} size={18} />
              ))}
            </span>
            <strong>{s.title}</strong>
            <span>{s.how}</span>
          </li>
        ))}
      </ol>
    </div>
  );
}
