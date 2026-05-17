export interface Column<T> {
  label: string;
  key: keyof T | string;
  render?: (row: T) => React.ReactNode;
}

interface DataTableProps<T extends Record<string, unknown>> {
  title?: string;
  columns: Column<T>[];
  rows: T[];
}

function formatValue(val: unknown): string {
  if (val === undefined || val === null || val === "") return "–";
  if (typeof val === "object") return JSON.stringify(val);
  return String(val);
}

export default function DataTable<T extends Record<string, unknown>>({
  title,
  columns,
  rows,
}: DataTableProps<T>) {
  return (
    <section className="result-block">
      {title && <h3>{title}</h3>}
      {rows.length === 0 ? (
        <p className="empty">No matching records.</p>
      ) : (
        <table>
          <thead>
            <tr>
              {columns.map((col) => (
                <th key={String(col.key)}>{col.label}</th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.map((row, i) => (
              <tr key={i}>
                {columns.map((col) => (
                  <td key={String(col.key)}>
                    {col.render
                      ? col.render(row)
                      : formatValue(row[col.key as keyof T])}
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
