import type { LogRecord } from "../../types/api";

interface LogHistogramProps {
  records: LogRecord[];
}

const BUCKETS = 20;
const W = 600, H = 150, pL = 38, pB = 26, pT = 18, pR = 8;
const cW = W - pL - pR, cH = H - pT - pB;
const barW = cW / BUCKETS;

type SevKey = "error" | "warn" | "info" | "debug";

const SEV_COLORS: Record<SevKey, string> = {
  error: "#f43f5e",
  warn: "#f59e0b",
  info: "#38bdf8",
  debug: "#95a6c6",
};

function sevKey(text: string): SevKey {
  const s = text.toUpperCase();
  if (s.includes("FATAL") || s.includes("ERROR")) return "error";
  if (s.includes("WARN")) return "warn";
  if (s.includes("DEBUG") || s.includes("TRACE")) return "debug";
  return "info";
}

function fmt(t: number): string {
  const d = new Date(t);
  return (
    String(d.getHours()).padStart(2, "0") +
    ":" +
    String(d.getMinutes()).padStart(2, "0")
  );
}

export default function LogHistogram({ records }: LogHistogramProps) {
  if (!records.length) return null;

  const timestamps = records
    .map((r) => new Date(r.timestamp).getTime())
    .filter((t) => !isNaN(t));

  if (!timestamps.length) return null;

  const minT = Math.min(...timestamps);
  const maxT = Math.max(...timestamps, minT + 1);
  const bucketMs = (maxT - minT) / BUCKETS || 60000;

  type Bucket = Record<SevKey, number> & { total: number };
  const data: Bucket[] = Array.from({ length: BUCKETS }, () => ({
    error: 0, warn: 0, info: 0, debug: 0, total: 0,
  }));

  records.forEach((r) => {
    const t = new Date(r.timestamp).getTime();
    if (isNaN(t)) return;
    const bi = Math.min(Math.floor((t - minT) / bucketMs), BUCKETS - 1);
    const k = sevKey(r.severity_text ?? "");
    data[bi][k]++;
    data[bi].total++;
  });

  const maxCount = Math.max(1, ...data.map((b) => b.total));

  const legend: [string, SevKey][] = [
    ["ERROR", "error"],
    ["WARN", "warn"],
    ["INFO", "info"],
    ["DEBUG", "debug"],
  ];

  return (
    <svg viewBox={`0 0 ${W} ${H}`} style={{ display: "block", width: "100%", maxHeight: 96 }}>
      {/* Grid */}
      {[0, 1, 2, 3, 4].map((i) => {
        const y = pT + cH - (i / 4) * cH;
        return (
          <g key={i}>
            <line
              x1={pL} y1={y} x2={pL + cW} y2={y}
              stroke="rgba(148,163,184,0.1)" strokeWidth={1} strokeDasharray="3,3"
            />
            <text x={pL - 4} y={y + 4} textAnchor="end" fill="#95a6c6" fontSize={10}>
              {Math.round((maxCount * i) / 4)}
            </text>
          </g>
        );
      })}

      {/* Bars */}
      {data.map((b, i) => {
        let yOff = 0;
        return (
          <g key={i}>
            {(["error", "warn", "info", "debug"] as SevKey[]).map((k) => {
              if (!b[k]) return null;
              const bH = (b[k] / maxCount) * cH;
              const rect = (
                <rect
                  key={k}
                  x={pL + i * barW + 1}
                  y={pT + cH - yOff - bH}
                  width={barW - 2}
                  height={bH}
                  fill={SEV_COLORS[k]}
                  rx={1}
                />
              );
              yOff += bH;
              return rect;
            })}
          </g>
        );
      })}

      {/* X-axis labels */}
      {[0, 1, 2, 3, 4].map((i) => (
        <text
          key={i}
          x={pL + (i / 4) * cW}
          y={H - 4}
          textAnchor="middle"
          fill="#95a6c6"
          fontSize={10}
        >
          {fmt(minT + (i / 4) * (maxT - minT))}
        </text>
      ))}

      {/* Legend */}
      {(() => {
        let lx = pL;
        return legend.map(([lbl, key]) => {
          const el = (
            <g key={key}>
              <rect x={lx} y={4} width={10} height={10} fill={SEV_COLORS[key]} rx={2} />
              <text x={lx + 13} y={13} fill="#95a6c6" fontSize={10}>
                {lbl}
              </text>
            </g>
          );
          lx += lbl.length * 6.5 + 22;
          return el;
        });
      })()}
    </svg>
  );
}
