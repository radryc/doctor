const STORAGE_PREFIX = "doctor.dashboards.hiddenBuiltIns";

function storageKey(tenant: string): string {
  return `${STORAGE_PREFIX}.${tenant || "default"}`;
}

/** Read the built-in dashboard IDs hidden by this browser for a tenant. */
export function readHiddenBuiltIns(tenant: string): Set<string> {
  try {
    const raw = localStorage.getItem(storageKey(tenant));
    if (!raw) return new Set();
    const parsed = JSON.parse(raw);
    if (!Array.isArray(parsed)) return new Set();
    return new Set(parsed.filter((value): value is string => typeof value === "string"));
  } catch {
    return new Set();
  }
}

/** Persist the built-in dashboard IDs hidden by this browser for a tenant. */
export function writeHiddenBuiltIns(tenant: string, ids: Set<string>): void {
  try {
    localStorage.setItem(storageKey(tenant), JSON.stringify([...ids].sort()));
  } catch {
    /* localStorage unavailable; hiding remains session-local */
  }
}
