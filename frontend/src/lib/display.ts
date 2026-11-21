// Presentation-only display-profile helpers for the Doctor UI. Canonical keys
// remain available because the UI sends X-Strata-Display-Keep-Canonical, so
// existing data accessors keep working while labels use displayKey().

const PROFILE_KEY = "strata.displayProfile";
const KEYS_KEY = "strata.displayKeys";

export function currentDisplayProfile(): string {
  try {
    return localStorage.getItem(PROFILE_KEY) ?? "";
  } catch {
    return "";
  }
}

export function displayHeaders(): Record<string, string> {
  const profile = currentDisplayProfile();
  if (!profile || profile === "canonical") return {};
  return {
    "X-Strata-Display-Profile": profile,
    "X-Strata-Display-Keep-Canonical": "1",
  };
}

function keyMap(): Record<string, string> {
  try {
    const raw = localStorage.getItem(KEYS_KEY);
    if (!raw) return {};
    const parsed = JSON.parse(raw);
    return parsed && typeof parsed === "object" ? parsed : {};
  } catch {
    return {};
  }
}

export function displayKey(canonical: string): string {
  return keyMap()[canonical] ?? canonical;
}

export function field(obj: Record<string, unknown> | null | undefined, canonical: string): unknown {
  if (!obj) return undefined;
  const mapped = keyMap()[canonical];
  if (mapped && mapped in obj) return obj[mapped];
  return obj[canonical];
}

export async function setDisplayProfile(profile: string): Promise<void> {
  try {
    if (!profile || profile === "canonical") {
      localStorage.removeItem(PROFILE_KEY);
      localStorage.removeItem(KEYS_KEY);
      return;
    }
    localStorage.setItem(PROFILE_KEY, profile);
    const res = await fetch("/v1/display-profiles", { headers: { Accept: "application/json" } });
    if (!res.ok) return;
    const body = await res.json();
    const found = (body.profiles ?? []).find((candidate: any) => candidate.id === profile);
    if (found?.keys) localStorage.setItem(KEYS_KEY, JSON.stringify(found.keys));
  } catch {
    /* presentation-only */
  }
}
