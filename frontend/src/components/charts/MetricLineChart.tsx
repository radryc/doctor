import type { MetricSeries } from "../../types/api";

interface MetricLineChartProps {
  series: MetricSeries[];
  metricName?: string;
}

const W = 600, H = 150, pL = 52, pB = 26, pT = 10, pR = 10;
const cW = W - pL - pR, cH = H - pT - pB;
const palette = ["#6d5efc", "#00ade4", "#22c55e", "#f59e0b", "#ef4444", "#a855f7", "#14b8a6", "#f97316"];

function fmt(t: number): string {
  const d = new Date(t);
  return (
    String(d.getHours()).padStart(2, "0") +
    ":" +
    String(d.getMinutes()).padStart(2, "0")
  );
}

export default function MetricLineChart({ series, metricName }: MetricLineChartProps) {
  const parsedSeries = series
    .map((item, index) => ({
      color: palette[index % palette.length],
      label: item.label,
      points: item.points
        .map((point) => ({ t: new Date(point.timestamp).getTime(), v: Number(point.value) }))
        .filter((point) => !isNaN(point.t) && !isNaN(point.v) && isFinite(point.v))
        .sort((left, right) => left.t - right.t),
    }))
    .filter((item) => item.points.length >= 2);

  if (parsedSeries.length === 0) return null;

  const allPoints = parsedSeries.flatMap((item) => item.points);
  const minT = Math.min(...allPoints.map((point) => point.t));
  const maxT = Math.max(...allPoints.map((point) => point.t));
  const vals = allPoints.map((point) => point.v);
  const minV = Math.min(...vals), maxV = Math.max(...vals);
  const rangeV = maxV - minV || 1;

  const px = (t: number) => pL + ((t - minT) / (maxT - minT || 1)) * cW;
  const py = (v: number) => pT + cH - ((v - minV) / rangeV) * cH;

  const gridLines = Array.from({ length: 5 }, (_, i) => i);
  const singleSeries = parsedSeries.length === 1 ? parsedSeries[0] : undefined;
  const areaD = singleSeries
    ? [
        `M ${px(singleSeries.points[0].t).toFixed(1)},${py(singleSeries.points[0].v).toFixed(1)}`,
        ...singleSeries.points.slice(1).map((point) => `L ${px(point.t).toFixed(1)},${py(point.v).toFixed(1)}`),
        `L ${px(singleSeries.points[singleSeries.points.length - 1].t).toFixed(1)},${(pT + cH).toFixed(1)}`,
        `L ${px(singleSeries.points[0].t).toFixed(1)},${(pT + cH).toFixed(1)}`,
        "Z",
      ].join(" ")
    : "";
  const avg = vals.reduce((sum, value) => sum + value, 0) / vals.length;
  const showDots = allPoints.length <= 160 && parsedSeries.every((item) => item.points.length <= 40);

  return (
    <section className="result-block">
      <h3>{metricName ? `Metric: ${metricName}` : "Metric values over time"}</h3>
      <div className="metric-chart-legend">
        {parsedSeries.map((item) => (
          <div className="metric-chart-legend__item" key={item.label} title={item.label}>
            <span className="metric-chart-legend__swatch" style={{ backgroundColor: item.color }} />
            <span className="metric-chart-legend__label">{item.label}</span>
          </div>
        ))}
      </div>
      <svg
        viewBox={`0 0 ${W} ${H}`}
        style={{ width: "100%", maxWidth: W, display: "block" }}
      >
        {/* Grid */}
        {gridLines.map((i) => {
          const v = minV + (i / 4) * rangeV;
          const y = py(v);
          return (
            <g key={i}>
              <line
                x1={pL} y1={y} x2={pL + cW} y2={y}
                stroke="rgba(148,163,184,0.1)" strokeWidth={1} strokeDasharray="3,3"
              />
              <text x={pL - 4} y={y + 4} textAnchor="end" fill="#95a6c6" fontSize={10}>
                {v.toFixed(Math.abs(v) < 10 ? 2 : 0)}
              </text>
            </g>
          );
        })}

        {/* Area */}
        {singleSeries && <path d={areaD} fill="rgba(109,94,252,0.16)" stroke="none" />}

        {parsedSeries.map((item) => {
          const linePoints = item.points
            .map((point) => `${px(point.t).toFixed(1)},${py(point.v).toFixed(1)}`)
            .join(" ");
          return (
            <g key={item.label}>
              <polyline
                points={linePoints}
                fill="none"
                stroke={item.color}
                strokeWidth={2}
                strokeLinejoin="round"
              />
              {showDots &&
                item.points.map((point, index) => (
                  <circle
                    key={`${item.label}-${index}`}
                    cx={px(point.t).toFixed(1)}
                    cy={py(point.v).toFixed(1)}
                    r={2.5}
                    fill={item.color}
                  />
                ))}
            </g>
          );
        })}

        {/* X-axis labels */}
        {gridLines.map((i) => (
          <text
            key={i}
            x={(pL + (i / 4) * cW).toFixed(1)}
            y={H - 4}
            textAnchor="middle"
            fill="#95a6c6"
            fontSize={10}
          >
            {fmt(minT + (i / 4) * (maxT - minT))}
          </text>
        ))}

        {/* Stats annotation */}
        <text
          x={pL + cW}
          y={pT + 10}
          textAnchor="end"
          fill="#95a6c6"
          fontSize={10}
        >
          {parsedSeries.length === 1
            ? `min:${minV.toFixed(2)}  avg:${avg.toFixed(2)}  max:${maxV.toFixed(2)}`
            : `series:${parsedSeries.length}  min:${minV.toFixed(2)}  max:${maxV.toFixed(2)}`}
        </text>
      </svg>
    </section>
  );
}
