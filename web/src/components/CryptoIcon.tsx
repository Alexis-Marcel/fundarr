import { monogram } from "../lib";

// Icons from cryptocurrency-icons (CC0), shipped with Fundarr: no request
// to a third party tells it which coins you hold.
const files = import.meta.glob<string>("../icons/*.svg", { eager: true, query: "?url", import: "default" });
const icons: Record<string, string> = {};
for (const [path, url] of Object.entries(files)) {
  icons[path.slice(path.lastIndexOf("/") + 1, -4).toUpperCase()] = url;
}

// The coin that stands for a chain of the Wallet source.
const chainCoin: Record<string, string> = {
  bitcoin: "BTC",
  evm: "ETH",
  solana: "SOL",
  xrp: "XRP",
  cardano: "ADA",
  tron: "TRX",
  litecoin: "LTC",
  dogecoin: "DOGE",
  bitcoincash: "BCH",
};

const aliases: Record<string, string> = { POL: "MATIC", WETH: "ETH", WBTC: "BTC", WSOL: "SOL" };

/** A coin's icon by symbol (BTC) or by Wallet chain kind (bitcoin). */
export function CryptoIcon({ symbol, kind, size = 22 }: { symbol?: string; kind?: string; size?: number }) {
  const sym = (symbol ?? chainCoin[kind ?? ""] ?? kind ?? "?").toUpperCase();
  const url = icons[aliases[sym] ?? sym];
  if (url) return <img className="crypto-icon" src={url} width={size} height={size} alt="" />;
  const [txt, hue] = monogram(sym);
  return (
    <span className="crypto-icon crypto-mono" style={{ width: size, height: size, ["--hue" as string]: hue }} aria-hidden>
      {txt.slice(0, 1)}
    </span>
  );
}

/** An asset as shown in tables: its icon, then its symbol (a security's name). */
export function AssetName({ asset, size = 18 }: { asset: { kind: string; symbol: string; isin?: string; name?: string }; size?: number }) {
  const named = asset.kind === "security" && asset.name;
  return (
    <span className="asset-name" title={named ? asset.isin || asset.symbol : undefined}>
      <CryptoIcon symbol={named ? asset.name : asset.symbol || asset.isin} size={size} />
      <strong className={named ? "clip" : undefined}>{named ? asset.name : asset.symbol || asset.isin || "?"}</strong>
    </span>
  );
}
