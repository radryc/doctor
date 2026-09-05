interface StatCardProps {
  label: string;
  value: string | number;
  sub?: string;
  variant?: "default" | "err" | "warn";
  valueStyle?: React.CSSProperties;
  children?: React.ReactNode;
}

export default function StatCard({
  label,
  value,
  sub,
  variant = "default",
  valueStyle,
  children,
}: StatCardProps) {
  const cls = "stat-card" + (variant !== "default" ? ` stat-card--${variant}` : "");
  return (
    <div className={cls}>
      <div className="stat-card__label">{label}</div>
      <div className="stat-card__value" style={valueStyle}>
        {children ?? value}
      </div>
      {sub && <div className="stat-card__sub">{sub}</div>}
    </div>
  );
}
