/**
 * Build a Guardian UI link that opens a specific partition.
 *
 * Guardian reads `partition` from the query string on load and selects that
 * partition in the overview, which is where the attention/failing assets are
 * listed. Keep this in one place so the dashboard, command palette, and future
 * surfaces produce the same deep link.
 */
export function guardianPartitionUrl(base: string, partition: string): string {
  const trimmed = base.trim();
  if (!trimmed) return "";
  try {
    const url = new URL(trimmed);
    url.searchParams.set("partition", partition);
    return url.toString();
  } catch {
    const separator = trimmed.includes("?") ? "&" : "?";
    return `${trimmed}${separator}partition=${encodeURIComponent(partition)}`;
  }
}
