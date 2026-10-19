import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  Activity,
  BarChart3,
  Copy,
  FileText,
  GitBranch,
  LayoutDashboard,
  Network,
  Search,
  ShieldCheck,
  Server,
  TriangleAlert,
} from "lucide-react";
import { api } from "../../api/client";
import { copyText } from "../../lib/clipboard";
import { useConfig } from "../../context/ConfigContext";
import type { GuardianOverview } from "../../types/api";

interface CommandPaletteProps {
  open: boolean;
  onClose: () => void;
}

interface PaletteCommand {
  id: string;
  group: string;
  label: string;
  hint?: string;
  keywords?: string;
  icon: React.ElementType;
  run: () => void;
}

const GROUP_ORDER = ["Go to", "Investigate", "Partitions", "Actions"];

function partitionTone(health: string | undefined): string {
  const value = (health ?? "").toLowerCase();
  if (value.includes("fail") || value.includes("crit") || value.includes("error")) return "failing";
  if (value.includes("attention") || value.includes("degrad") || value.includes("drift")) return "attention";
  if (value.includes("pending")) return "pending";
  return "healthy";
}

export default function CommandPalette({ open, onClose }: CommandPaletteProps) {
  const navigate = useNavigate();
  const { config } = useConfig();
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(0);
  const [partitions, setPartitions] = useState<GuardianOverview["partitions"]>([]);
  const [notice, setNotice] = useState("");
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (!open) return;
    setQuery("");
    setActive(0);
    setNotice("");
    const timer = window.setTimeout(() => inputRef.current?.focus(), 20);
    api
      .guardianOverview()
      .then((overview) => setPartitions(overview.partitions ?? []))
      .catch(() => setPartitions([]));
    return () => window.clearTimeout(timer);
  }, [open]);

  const commands = useMemo<PaletteCommand[]>(() => {
    const go = (to: string) => () => {
      onClose();
      navigate(to);
    };
    const items: PaletteCommand[] = [
      {
        id: "nav-oncall",
        group: "Go to",
        label: "On-call overview",
        hint: "g d",
        keywords: "dashboard home triage incidents",
        icon: LayoutDashboard,
        run: go("/"),
      },
      {
        id: "nav-services",
        group: "Go to",
        label: "Services",
        hint: "g s",
        keywords: "discovery red signals",
        icon: Network,
        run: go("/services"),
      },
      {
        id: "nav-traces",
        group: "Go to",
        label: "Traces",
        hint: "g t",
        keywords: "spans waterfall latency",
        icon: GitBranch,
        run: go("/traces"),
      },
      {
        id: "nav-logs",
        group: "Go to",
        label: "Logs",
        hint: "g l",
        keywords: "errors search tail",
        icon: FileText,
        run: go("/logs"),
      },
      {
        id: "nav-metrics",
        group: "Go to",
        label: "Metrics",
        hint: "g m",
        keywords: "promql prometheus graphs",
        icon: BarChart3,
        run: go("/metrics"),
      },
      {
        id: "nav-fleet",
        group: "Go to",
        label: "Fleet topology",
        hint: "g f",
        keywords: "assets partitions hubs",
        icon: Server,
        run: go("/fleet"),
      },
      {
        id: "inv-errors",
        group: "Investigate",
        label: "Errors in the last 15 minutes",
        hint: "15m",
        keywords: "logs error fatal exception",
        icon: TriangleAlert,
        run: go("/logs?range=15&sev=ERR"),
      },
      {
        id: "inv-traces",
        group: "Investigate",
        label: "Recent traces",
        hint: "15m",
        keywords: "latency slow spans",
        icon: Activity,
        run: go("/traces?range=15"),
      },
      {
        id: "inv-live-logs",
        group: "Investigate",
        label: "Live logs (1h window)",
        keywords: "tail follow streaming",
        icon: FileText,
        run: go("/logs?range=60"),
      },
      {
        id: "act-copy-link",
        group: "Actions",
        label: "Copy current view link",
        keywords: "share url clipboard incident",
        icon: Copy,
        run: () => {
          void copyText(window.location.href).then((ok) => {
            setNotice(ok ? "Current view link copied" : "Could not copy link");
          });
        },
      },
    ];

    if (config.guardian_url) {
      items.push({
        id: "act-guardian",
        group: "Actions",
        label: "Open Guardian control plane",
        keywords: "guardian reconcile deployment",
        icon: ShieldCheck,
        run: () => {
          onClose();
          window.open(config.guardian_url, "_blank", "noreferrer");
        },
      });
    }

    const sorted = [...partitions].sort((a, b) => {
      const rank = (health: string) => {
        const tone = partitionTone(health);
        return tone === "failing" ? 0 : tone === "attention" ? 1 : tone === "pending" ? 2 : 3;
      };
      return rank(a.health) - rank(b.health) || a.name.localeCompare(b.name);
    });

    for (const partition of sorted.slice(0, 60)) {
      const name = partition.name;
      const encoded = encodeURIComponent(name);
      const tone = partitionTone(partition.health);
      const hint = `${tone} · ${partition.failingAssets ?? 0} failing`;
      items.push(
        {
          id: `part-assets-${name}`,
          group: "Partitions",
          label: `${name} · assets`,
          hint,
          keywords: `partition ${name} ${partition.displayStatus ?? ""}`,
          icon: Server,
          run: go(`/partition/${encoded}/assets`),
        },
        {
          id: `part-logs-${name}`,
          group: "Partitions",
          label: `${name} · error logs`,
          hint: "15m",
          keywords: `partition ${name} logs errors`,
          icon: FileText,
          run: go(`/logs?partition=${encoded}&range=15&sev=ERR`),
        },
        {
          id: `part-traces-${name}`,
          group: "Partitions",
          label: `${name} · traces`,
          hint: "15m",
          keywords: `partition ${name} traces spans`,
          icon: GitBranch,
          run: go(`/traces?partition=${encoded}&range=15`),
        },
      );
    }
    return items;
  }, [config.guardian_url, navigate, onClose, partitions]);

  const filtered = useMemo(() => {
    const needle = query.trim().toLowerCase();
    const matched = needle
      ? commands.filter((command) =>
          `${command.group} ${command.label} ${command.hint ?? ""} ${command.keywords ?? ""}`
            .toLowerCase()
            .includes(needle)
        )
      : commands;
    return matched.slice(0, 60);
  }, [commands, query]);

  const grouped = useMemo(() => {
    const map = new Map<string, PaletteCommand[]>();
    for (const command of filtered) {
      if (!map.has(command.group)) map.set(command.group, []);
      map.get(command.group)!.push(command);
    }
    return GROUP_ORDER.filter((group) => map.has(group)).map((group) => ({
      group,
      items: map.get(group)!,
    }));
  }, [filtered]);

  useEffect(() => {
    setActive(0);
  }, [query]);

  useEffect(() => {
    if (active >= filtered.length) setActive(Math.max(0, filtered.length - 1));
  }, [active, filtered.length]);

  useEffect(() => {
    if (!open) return;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        event.preventDefault();
        onClose();
        return;
      }
      if (event.key === "ArrowDown") {
        event.preventDefault();
        setActive((current) => Math.min(current + 1, filtered.length - 1));
        return;
      }
      if (event.key === "ArrowUp") {
        event.preventDefault();
        setActive((current) => Math.max(current - 1, 0));
        return;
      }
      if (event.key === "Enter") {
        event.preventDefault();
        filtered[active]?.run();
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [active, filtered, onClose, open]);

  if (!open) return null;

  let flatIndex = -1;

  return (
    <div
      className="command-overlay"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) onClose();
      }}
    >
      <div className="command-modal" role="dialog" aria-modal="true" aria-label="Command palette">
        <div className="command-search">
          <Search size={17} aria-hidden="true" />
          <input
            ref={inputRef}
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder="Jump to a page, partition, or investigation…"
            aria-label="Search commands"
            spellCheck={false}
          />
          <kbd>esc</kbd>
        </div>
        <div className="command-list" role="listbox" aria-label="Commands">
          {grouped.length === 0 ? (
            <div className="command-empty">No commands match “{query}”.</div>
          ) : (
            grouped.map(({ group, items }) => (
              <div key={group}>
                <div className="command-group-label">{group}</div>
                {items.map((command) => {
                  flatIndex += 1;
                  const index = flatIndex;
                  const Icon = command.icon;
                  const isActive = index === active;
                  return (
                    <button
                      key={command.id}
                      type="button"
                      role="option"
                      aria-selected={isActive}
                      className={"command-item" + (isActive ? " is-active" : "")}
                      onMouseEnter={() => setActive(index)}
                      onClick={() => command.run()}
                    >
                      <Icon className="command-item__icon" size={15} aria-hidden="true" />
                      <span className="command-item__label">{command.label}</span>
                      {command.hint && <span className="command-item__hint">{command.hint}</span>}
                    </button>
                  );
                })}
              </div>
            ))
          )}
        </div>
        <div className="command-footer">
          <span>
            <kbd>↑</kbd> <kbd>↓</kbd> navigate
          </span>
          <span>
            <kbd>↵</kbd> open
          </span>
          <span>
            <kbd>g</kbd> then <kbd>d</kbd>/<kbd>s</kbd>/<kbd>t</kbd>/<kbd>l</kbd>/<kbd>m</kbd>/<kbd>f</kbd> jump
          </span>
          <span>{notice || `${filtered.length} commands`}</span>
        </div>
      </div>
    </div>
  );
}
