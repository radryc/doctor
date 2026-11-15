export function agentBase(): string {
  const runtime = (window as unknown as { STRATA_AGENT_URL?: string })
    .STRATA_AGENT_URL;
  const configured =
    runtime || (import.meta.env.VITE_AGENT_URL as string | undefined);
  const host = window.location.hostname || "localhost";
  const proto = window.location.protocol === "https:" ? "https:" : "http:";
  return (configured || `${proto}//${host}:9191`).replace(/\/$/, "");
}

export function openAgentChat(path: string): void {
  window.open(`${agentBase()}${path}`, "_blank", "noopener,noreferrer");
}
