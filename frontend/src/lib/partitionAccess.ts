/**
 * Partition access helpers shared by the alerts UI.
 *
 * `accessible` is the list returned by /v1/ui/config. `undefined` means the
 * identity is unrestricted (admin or access enforcement disabled), while an
 * empty array means the identity has no readable partitions.
 */

export function isPartitionDenied(partition: string, accessible?: string[]): boolean {
  if (!partition || !accessible) return false;
  return !accessible.includes(partition);
}

export function pickDefaultPartition(configured: string, accessible?: string[]): string {
  if (!accessible) return configured;
  if (accessible.length === 0) return "";
  if (configured && accessible.includes(configured)) return configured;
  return accessible[0];
}
