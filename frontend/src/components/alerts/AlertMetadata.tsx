import { useMemo, useState } from "react";
import { Check, Code2, Copy, ExternalLink } from "lucide-react";

interface AlertMetadataProps {
  labels: Record<string, string>;
  annotations?: Record<string, string>;
}

interface MetaEntry {
  key: string;
  group: string;
  label: string;
  value: string;
}

const PRIMARY_KEYS = [
  "alertname",
  "severity",
  "summary",
  "description",
  "status",
  "health",
  "message",
  "error",
  "errors",
];

const STATE_VALUES = new Set([
  "ok",
  "healthy",
  "passing",
  "stable",
  "available",
  "ready",
  "up",
  "error",
  "failing",
  "unavailable",
  "degraded",
  "critical",
  "warning",
  "info",
  "unknown",
  "resolved",
  "firing",
  "pending",
]);

function humanizeToken(token: string): string {
  const words = token
    .replace(/[._-]+/g, " ")
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .trim()
    .split(/\s+/)
    .filter(Boolean);
  if (words.length === 0) return token;
  return words
    .map((word, index) =>
      index === 0 && /^[a-z]/.test(word)
        ? word.charAt(0).toUpperCase() + word.slice(1)
        : word
    )
    .join(" ");
}

function toEntry(key: string, value: string): MetaEntry {
  const dot = key.indexOf(".");
  if (dot > 0 && dot < key.length - 1) {
    return {
      key,
      group: humanizeToken(key.slice(0, dot)),
      label: humanizeToken(key.slice(dot + 1)),
      value,
    };
  }
  return { key, group: "", label: humanizeToken(key), value };
}

function sortEntries(entries: MetaEntry[]): MetaEntry[] {
  const rank = (entry: MetaEntry) => {
    const base = entry.key.slice(entry.key.lastIndexOf(".") + 1).toLowerCase();
    const index = PRIMARY_KEYS.indexOf(base);
    return index === -1 ? PRIMARY_KEYS.length : index;
  };
  return [...entries].sort((a, b) => {
    const byGroup = a.group.localeCompare(b.group);
    if (byGroup !== 0) return byGroup;
    const byRank = rank(a) - rank(b);
    if (byRank !== 0) return byRank;
    return a.key.localeCompare(b.key);
  });
}

function stateClass(value: string): string {
  switch (value.toLowerCase()) {
    case "ok":
    case "healthy":
    case "passing":
    case "stable":
    case "available":
    case "ready":
    case "up":
    case "resolved":
      return "border-emerald-500/40 bg-emerald-500/10 text-emerald-300";
    case "critical":
    case "error":
    case "failing":
    case "unavailable":
    case "firing":
      return "border-rose-500/40 bg-rose-500/10 text-rose-300";
    case "warning":
    case "degraded":
    case "pending":
      return "border-amber-500/40 bg-amber-500/10 text-amber-300";
    default:
      return "border-slate-600 bg-slate-800/70 text-slate-300";
  }
}

function isUrl(value: string): boolean {
  return /^https?:\/\//i.test(value.trim());
}

function isLongText(key: string, value: string): boolean {
  return (
    value.length > 60 ||
    /(error|errors|message|description|details|traceback|stack)/i.test(key)
  );
}

function CopyButton({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      type="button"
      title="Copy JSON"
      onClick={() => {
        void navigator.clipboard?.writeText(text).then(() => {
          setCopied(true);
          window.setTimeout(() => setCopied(false), 1200);
        });
      }}
      className="inline-flex items-center gap-1 rounded border border-slate-700 bg-slate-900 px-1.5 py-1 text-[10px] text-slate-400 hover:border-cyan-500/40 hover:text-cyan-300"
    >
      {copied ? <Check size={11} /> : <Copy size={11} />}
      {copied ? "Copied" : "Copy"}
    </button>
  );
}

function RawJson({ data }: { data: Record<string, string> }) {
  const text = useMemo(() => JSON.stringify(data ?? {}, null, 2), [data]);
  return (
    <div className="space-y-2">
      <div className="flex justify-end">
        <CopyButton text={text} />
      </div>
      <pre className="max-h-48 overflow-auto rounded border border-slate-800 bg-slate-950 p-2 text-[10px] leading-relaxed text-slate-300">
        {text}
      </pre>
    </div>
  );
}

function MetadataSection({
  title,
  data,
}: {
  title: string;
  data: Record<string, string>;
}) {
  const [showRaw, setShowRaw] = useState(false);
  const entries = useMemo(
    () =>
      sortEntries(
        Object.entries(data ?? {}).map(([key, value]) =>
          toEntry(
            key,
            typeof value === "string" ? value : JSON.stringify(value) ?? String(value)
          )
        )
      ),
    [data]
  );

  const groups: Array<{ name: string; entries: MetaEntry[] }> = [];
  for (const entry of entries) {
    const last = groups[groups.length - 1];
    if (last && last.name === entry.group) last.entries.push(entry);
    else groups.push({ name: entry.group, entries: [entry] });
  }

  return (
    <section className="mb-5 last:mb-0">
      <div className="mb-2 flex items-center justify-between gap-2">
        <h3 className="text-[10px] font-bold uppercase tracking-wide text-slate-500">
          {title}
          <span className="ml-1.5 font-normal normal-case tracking-normal text-slate-600">
            {entries.length}
          </span>
        </h3>
        {entries.length > 0 && (
          <button
            type="button"
            onClick={() => setShowRaw((value) => !value)}
            className="inline-flex items-center gap-1 text-[10px] text-slate-500 hover:text-cyan-300"
            title={showRaw ? "Show parsed values" : "Show raw JSON"}
          >
            <Code2 size={11} />
            {showRaw ? "Parsed" : "JSON"}
          </button>
        )}
      </div>

      {entries.length === 0 ? (
        <p className="text-[11px] text-slate-600">None</p>
      ) : showRaw ? (
        <RawJson data={data} />
      ) : (
        <div className="space-y-3">
          {groups.map((group) => (
            <div key={group.name || "root"}>
              {group.name && (
                <div className="mb-1 text-[9px] font-semibold uppercase tracking-[0.14em] text-slate-600">
                  {group.name}
                </div>
              )}
              <dl className="grid grid-cols-[minmax(0,6.5rem)_minmax(0,1fr)] gap-x-3 gap-y-1.5">
                {group.entries.map((entry) => (
                  <div key={entry.key} className="contents">
                    <dt
                      className="truncate text-[10px] text-slate-500"
                      title={entry.key}
                    >
                      {entry.label}
                    </dt>
                    <dd className="min-w-0 text-[11px] leading-snug text-slate-300">
                      {isUrl(entry.value) ? (
                        <a
                          href={entry.value}
                          target="_blank"
                          rel="noreferrer"
                          className="inline-flex items-center gap-1 break-all text-cyan-300 hover:text-cyan-200"
                        >
                          {entry.value}
                          <ExternalLink size={10} className="shrink-0" />
                        </a>
                      ) : STATE_VALUES.has(entry.value.toLowerCase()) ? (
                        <span
                          className={`inline-block rounded border px-1.5 py-0.5 text-[10px] font-semibold uppercase ${stateClass(entry.value)}`}
                        >
                          {entry.value}
                        </span>
                      ) : isLongText(entry.key, entry.value) ? (
                        <code className="block whitespace-pre-wrap break-words rounded border border-slate-800 bg-slate-950/70 px-1.5 py-1 font-mono text-[10px] text-slate-300">
                          {entry.value}
                        </code>
                      ) : (
                        <span className="break-words">{entry.value}</span>
                      )}
                    </dd>
                  </div>
                ))}
              </dl>
            </div>
          ))}
        </div>
      )}
    </section>
  );
}

export default function AlertMetadata({ labels, annotations }: AlertMetadataProps) {
  return (
    <div>
      <MetadataSection title="Labels" data={labels} />
      <MetadataSection title="Annotations" data={annotations ?? {}} />
    </div>
  );
}
