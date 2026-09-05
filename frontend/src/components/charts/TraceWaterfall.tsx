import type { TraceRecord } from "../../types/api";

interface TraceWaterfallProps {
  spans: TraceRecord[];
}

const W = 600, H_PER_ROW = 22, pL = 165, pT = 10, pB = 28, pR = 10;
const MAX_ROWS = 28;
const PALETTE = ["#6d5efc", "#10b981", "#f59e0b", "#f43f5e", "#c084fc", "#14b8a6", "#38bdf8"];

function fmtDur(ms: number): string {
  return ms >= 1000 ? `${(ms / 1000).toFixed(1)}s` : `${Math.round(ms)}ms`;
}

export default function TraceWaterfall({ spans }: TraceWaterfallProps) {
  if (!spans.length) return null;

  const parsed = spans
    .map((s) => ({
      name: s.name || s.span_id || "span",
      service: s.service ?? "",
      start: new Date(s.start_time).getTime(),
      end: new Date(s.end_time).getTime(),
      status: s.status_code ?? "",
    }))
    .filter((s) => !isNaN(s.start) && !isNaN(s.end))
    .sort((a, b) => a.start - b.start);

  if (!parsed.length) return null;

  const traceStart = parsed[0].start;
  const traceEnd = Math.max(...parsed.map((s) => s.end));
  const dur = traceEnd - traceStart || 1;

  const services = [...new Set(parsed.map((s) => s.service))];
  const svcColors: Record<string, string> = {};
  services.forEach((s, i) => {
    svcColors[s] = PALETTE[i % PALETTE.length];
  });

  const rows = parsed.slice(0, MAX_ROWS);
  const cW = W - pL - pR;
  const H = pT + rows.length * H_PER_ROW + pB;

  const px = (t: number) => pL + ((t - traceStart) / dur) * cW;

  return (
    <section className="result-block">
      <h3>{`Trace waterfall — total ${fmtDur(dur)}`}</h3>
      <svg
        viewBox={`0 0 ${W} ${H}`}
        style={{ width: "100%", maxWidth: W, display: "block" }}
      >
        {/* Grid lines + x-axis labels */}
        {[0, 1, 2, 3, 4].map((i) => {
          const x = (pL + (i / 4) * cW).toFixed(1);
          const ms = (i / 4) * dur;
          return (
            <g key={i}>
              <line
                x1={x} y1={pT} x2={x} y2={H - pB}
                stroke="rgba(148,163,184,0.1)" strokeWidth={1} strokeDasharray="3,3"
              />
              <text x={x} y={H - 4} textAnchor="middle" fill="#95a6c6" fontSize={10}>
                {fmtDur(ms)}
              </text>
            </g>
          );
        })}

        {/* Spans */}
        {rows.map((span, i) => {
          const y = pT + i * H_PER_ROW;
          const color = span.status.toLowerCase().includes("error")
            ? "#f43f5e"
            : (svcColors[span.service] ?? "#6d5efc");
          const x1 = px(span.start);
          const bW = Math.max(2, ((span.end - span.start) / dur) * cW);
          const d = span.end - span.start;
          const label = span.name.length > 24 ? span.name.slice(0, 22) + "\u2026" : span.name;

          return (
            <g key={i}>
              <text
                x={pL - 6}
                y={y + H_PER_ROW * 0.62}
                textAnchor="end"
                fill="#95a6c6"
                fontSize={10}
              >
                {label}
              </text>
              <rect
                x={x1.toFixed(1)}
                y={y + 4}
                width={bW.toFixed(1)}
                height={H_PER_ROW - 8}
                fill={color}
                rx={2}
                opacity={0.85}
              />
              <text
                x={(x1 + bW + 3).toFixed(1)}
                y={y + H_PER_ROW * 0.62}
                fill="#e7eefc"
                fontSize={9}
              >
                {fmtDur(d)}
              </text>
              <line
                x1={0} y1={y + H_PER_ROW} x2={W} y2={y + H_PER_ROW}
                stroke="rgba(148,163,184,0.12)" strokeWidth={1}
              />
            </g>
          );
        })}
      </svg>
    </section>
  );
}
