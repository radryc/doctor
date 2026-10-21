import { useCallback, useEffect, useRef, useState } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import {
  Plus,
  Download,
  Upload,
  Copy,
  Trash2,
  ChevronLeft,
  Pencil,
  Save,
  X,
} from "lucide-react";
import { api } from "../api/client";
import { useConfig } from "../context/ConfigContext";
import type { DashboardDocument, DashboardWidget, VariableValues } from "../types/dashboard";
import { downloadJSON } from "../lib/download";
import { variablesToParams, variablesFromParams } from "../lib/variables";
import DashboardVariableBar from "../components/dashboards/DashboardVariableBar";
import DashboardWidgetCard from "../components/dashboards/DashboardWidgetCard";
import ImportExportModal from "../components/dashboards/ImportExportModal";
import WidgetEditorModal from "../components/dashboards/WidgetEditorModal";

// ── Dashboard browser (list view at /dashboards) ─────────────────────────────

export default function DashboardsPage() {
  const { id } = useParams<{ id?: string }>();

  if (id) {
    if (id === "new") return <NewDashboard />;
    return <DashboardView id={id} />;
  }
  return <DashboardBrowser />;
}

function downloadDashboard(tenant: string, id: string, format: "json" | "yaml") {
  const anchor = document.createElement("a");
  anchor.href = api.dashboardDownloadURL(tenant, id, format);
  anchor.download = `${id}.${format}`;
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
}

function DashboardBrowser() {
  const { config } = useConfig();
  const navigate = useNavigate();
  const [docs, setDocs] = useState<DashboardDocument[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [search, setSearch] = useState("");
  const [showImport, setShowImport] = useState(false);

  const load = useCallback(() => {
    setLoading(true);
    setError("");
    const params = new URLSearchParams({ tenant: config.default_tenant });
    api
      .listDashboards(params)
      .then((r) => setDocs(r.dashboards ?? []))
      .catch((err) => setError(err instanceof Error ? err.message : "Failed to load dashboards"))
      .finally(() => setLoading(false));
  }, [config.default_tenant]);

  useEffect(() => { load(); }, [load]);

  const filtered = docs.filter((d) => {
    if (!search) return true;
    const q = search.toLowerCase();
    return (
      d.title.toLowerCase().includes(q) ||
      d.id.toLowerCase().includes(q) ||
      (d.tags ?? []).some((t) => t.toLowerCase().includes(q))
    );
  });

  const deleteDashboard = async (doc: DashboardDocument) => {
    if (!confirm(`Delete "${doc.title}"?`)) return;
    try {
      await api.deleteDashboard(config.default_tenant, doc.id);
      load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Delete failed");
    }
  };

  const cloneDashboard = async (doc: DashboardDocument) => {
    const cloned: DashboardDocument = {
      ...doc,
      id: `${doc.id}-copy-${Date.now()}`,
      title: `${doc.title} (copy)`,
      builtIn: false,
      tenant: config.default_tenant,
      meta: undefined,
    };
    try {
      await api.putDashboard(config.default_tenant, cloned.id, cloned);
      load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Clone failed");
    }
  };

  const exportAll = (format: "json" | "yaml") => {
    const params = new URLSearchParams({ tenant: config.default_tenant, format });
    window.location.href = api.exportDashboards(params);
  };

  return (
    <div className="tab-panel">
      <div className="panel-header">
        <div className="panel-header__left">
          <h2>Dashboards</h2>
          <p>{loading ? "Loading…" : `${filtered.length} dashboard${filtered.length !== 1 ? "s" : ""}`}</p>
        </div>
        <div className="xp-toolbar">
          <input
            type="text"
            className="xp-search"
            placeholder="Search dashboards…"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
          <button className="xp-live-btn" onClick={() => setShowImport(true)}>
            <Upload size={12} /> Import
          </button>
          <button className="xp-live-btn" onClick={() => exportAll("json")}>
            <Download size={12} /> Export JSON
          </button>
          <button className="xp-live-btn" onClick={() => exportAll("yaml")}>
            <Download size={12} /> Export YAML
          </button>
          <button className="btn btn--primary" onClick={() => navigate("/dashboards/new")}>
            <Plus size={12} /> New
          </button>
        </div>
      </div>

      <div className="panel-body dashboard-browser">
        {error && <div className="dash-error">{error}</div>}
        {loading ? (
          <div className="xp-empty">Loading…</div>
        ) : filtered.length === 0 ? (
          <div className="xp-empty">
            No dashboards found. <button className="btn btn--link" onClick={() => navigate("/dashboards/new")}>Create one</button>
          </div>
        ) : (
          <div className="dash-browser-grid">
            {filtered.map((doc) => (
              <DashboardCard
                key={doc.id}
                doc={doc}
                tenant={config.default_tenant}
                onOpen={() => navigate(`/dashboards/${encodeURIComponent(doc.id)}`)}
                onClone={() => void cloneDashboard(doc)}
                onDelete={() => void deleteDashboard(doc)}
              />
            ))}
          </div>
        )}
      </div>

      {showImport && (
        <ImportExportModal
          tenant={config.default_tenant}
          onClose={() => setShowImport(false)}
          onImported={load}
        />
      )}
    </div>
  );
}

interface CardProps {
  doc: DashboardDocument;
  tenant: string;
  onOpen: () => void;
  onClone: () => void;
  onDelete: () => void;
}

function DashboardCard({ doc, tenant, onOpen, onClone, onDelete }: CardProps) {
  return (
    <div className="dash-card" onClick={onOpen}>
      <div className="dash-card__header">
        <span className="dash-card__title">{doc.title}</span>
        {doc.builtIn && <span className="dash-card__badge">Built-in</span>}
      </div>
      {doc.description && <p className="dash-card__desc">{doc.description}</p>}
      {(doc.tags ?? []).length > 0 && (
        <div className="dash-card__tags">
          {doc.tags!.map((t) => <span key={t} className="dash-card__tag">{t}</span>)}
        </div>
      )}
      <div className="dash-card__meta">
        {(doc.layout ?? []).length} widget{(doc.layout ?? []).length === 1 ? "" : "s"}
        {doc.defaultTimeRange ? ` · ${doc.defaultTimeRange}` : ""}
      </div>
      <div className="dash-card__actions" onClick={(e) => e.stopPropagation()}>
        <button className="icon-btn" title="Clone to shared dashboard" onClick={onClone}>
          <Copy size={12} />
        </button>
        <button className="icon-btn" title="Download JSON" onClick={() => downloadJSON(doc, `${doc.id}.json`)}>
          <Download size={12} />
        </button>
        <button className="icon-btn" title="Download YAML" onClick={() => downloadDashboard(tenant, doc.id, "yaml")}>
          <Download size={12} />Y
        </button>
        {!doc.builtIn && (
          <button className="icon-btn icon-btn--danger" title="Delete" onClick={onDelete}>
            <Trash2 size={12} />
          </button>
        )}
      </div>
    </div>
  );
}

// ── New dashboard ───────────────────────────────────────────────────────────

function NewDashboard() {
  const { config } = useConfig();
  const navigate = useNavigate();
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [tags, setTags] = useState("");
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);

  const create = async () => {
    if (!title.trim()) {
      setError("Give the dashboard a title.");
      return;
    }
    setSaving(true);
    setError("");
    const baseID = slugify(title) || "dashboard";
    const id = `${baseID}-${Date.now().toString(36)}`;
    const doc: DashboardDocument = {
      version: "doctor.dashboard/v1",
      id,
      title: title.trim(),
      description: description.trim() || undefined,
      tenant: config.default_tenant,
      tags: tags.split(",").map((t) => t.trim()).filter(Boolean),
      defaultTimeRange: "1h",
      variables: [
        { name: "partition", label: "Partition", type: "partition", defaultValue: "$__all", includeAll: true },
        { name: "service", label: "Service", type: "service", defaultValue: "$__all", includeAll: true },
      ],
      layout: [],
    };
    try {
      await api.putDashboard(config.default_tenant, id, doc);
      navigate(`/dashboards/${encodeURIComponent(id)}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not create dashboard");
      setSaving(false);
    }
  };

  return (
    <div className="tab-panel">
      <div className="panel-header">
        <div className="panel-header__left" style={{ display: "flex", alignItems: "center", gap: "0.5rem" }}>
          <button className="icon-btn" onClick={() => navigate("/dashboards")} title="Back to dashboards">
            <ChevronLeft size={16} />
          </button>
          <h2>New dashboard</h2>
        </div>
      </div>
      <div className="panel-body new-dashboard">
        <div className="new-dashboard__form">
          <label>
            Title
            <input value={title} onChange={(e) => setTitle(e.target.value)} placeholder="Checkout service RED" autoFocus />
          </label>
          <label>
            Description
            <input value={description} onChange={(e) => setDescription(e.target.value)} placeholder="What should this dashboard answer?" />
          </label>
          <label>
            Tags
            <input value={tags} onChange={(e) => setTags(e.target.value)} placeholder="apm, production, on-call" />
          </label>
          {error && <div className="import-error">{error}</div>}
          <div className="new-dashboard__actions">
            <button className="btn" onClick={() => navigate("/dashboards")}>Cancel</button>
            <button className="btn btn--primary" onClick={() => void create()} disabled={saving}>
              {saving ? "Creating…" : "Create dashboard"}
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

// ── Dashboard view (rendered at /dashboards/:id) ─────────────────────────────

function DashboardView({ id }: { id: string }) {
  const { config } = useConfig();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const searchParamsRef = useRef(searchParams);
  searchParamsRef.current = searchParams;
  const [doc, setDoc] = useState<DashboardDocument | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [saveMessage, setSaveMessage] = useState("");
  const [variableValues, setVariableValues] = useState<VariableValues>({});
  const [editMode, setEditMode] = useState(false);
  const [editorWidget, setEditorWidget] = useState<DashboardWidget | null | undefined>(undefined);
  const [saving, setSaving] = useState(false);

  const load = useCallback(() => {
    setLoading(true);
    setError("");
    api
      .getDashboard(config.default_tenant, id)
      .then((d) => {
        setDoc(d);
        // Initialize variable values: URL params override defaults.
        const fromURL = variablesFromParams(searchParamsRef.current);
        const defaults: VariableValues = {};
        for (const v of d.variables ?? []) {
          defaults[v.name] = fromURL[v.name] ?? v.defaultValue;
        }
        setVariableValues(defaults);
      })
      .catch((e) => setError(e instanceof Error ? e.message : "Failed to load dashboard"))
      .finally(() => setLoading(false));
  }, [id, config.default_tenant]);

  useEffect(() => { load(); }, [load]);

  const handleVariableChange = (vals: VariableValues) => {
    setVariableValues(vals);
    setSearchParams(variablesToParams(vals), { replace: true });
  };

  const save = async (next: DashboardDocument) => {
    setSaving(true);
    setError("");
    setSaveMessage("");
    try {
      await api.putDashboard(config.default_tenant, next.id, next, next.meta?.versionId);
      // The server issues a new CAS version on every write; clear the local
      // version so the next save starts from the latest revision.
      setDoc({ ...next, meta: undefined });
      setEditMode(false);
      setSaveMessage("Saved");
      window.setTimeout(() => setSaveMessage(""), 1800);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Save failed");
    } finally {
      setSaving(false);
    }
  };

  const updateLayout = (layout: DashboardWidget[]) => {
    if (!doc) return;
    setDoc({ ...doc, layout });
  };

  const saveWidget = (widget: DashboardWidget) => {
    if (!doc) return;
    const layout = doc.layout ?? [];
    const exists = layout.some((w) => w.id === widget.id);
    updateLayout(exists ? layout.map((w) => (w.id === widget.id ? widget : w)) : [...layout, widget]);
    setEditorWidget(undefined);
    setEditMode(true);
  };

  const removeWidget = (widgetID: string) => {
    if (!doc) return;
    updateLayout((doc.layout ?? []).filter((w) => w.id !== widgetID));
  };

  const clone = async () => {
    if (!doc) return;
    const cloned: DashboardDocument = {
      ...doc,
      id: `${doc.id}-copy-${Date.now()}`,
      title: `${doc.title} (copy)`,
      builtIn: false,
      meta: undefined,
    };
    try {
      await api.putDashboard(config.default_tenant, cloned.id, cloned);
      navigate(`/dashboards/${encodeURIComponent(cloned.id)}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Clone failed");
    }
  };

  if (loading) return <div className="tab-panel"><div className="xp-empty">Loading dashboard…</div></div>;
  if (error && !doc) return <div className="tab-panel"><div className="xp-empty">Error: {error}</div></div>;
  if (!doc) return null;

  const layout = doc.layout ?? [];
  const maxRow = layout.reduce((max, w) => Math.max(max, w.gridPos.y + w.gridPos.h), 4);
  const hasUnsavedChanges = editMode;

  return (
    <div className="tab-panel">
      <div className="panel-header">
        <div className="panel-header__left" style={{ display: "flex", alignItems: "center", gap: "0.5rem" }}>
          <button className="icon-btn" onClick={() => navigate("/dashboards")} title="Back to dashboards">
            <ChevronLeft size={16} />
          </button>
          <h2>{doc.title}</h2>
          {doc.builtIn && <span className="dash-card__badge">Built-in Preset</span>}
          {saveMessage && <span className="dash-saved">{saveMessage}</span>}
        </div>
        <div className="xp-toolbar">
          {error && <span className="dash-error dash-error--inline">{error}</span>}
          <button className="xp-live-btn" onClick={() => downloadJSON(doc, `${doc.id}.json`)}>
            <Download size={12} /> JSON
          </button>
          <button className="xp-live-btn" onClick={() => downloadDashboard(config.default_tenant, doc.id, "yaml")}>
            <Download size={12} /> YAML
          </button>
          {doc.builtIn ? (
            <button className="xp-live-btn" onClick={() => void clone()} title="Clone this preset into an editable dashboard">
              <Copy size={12} /> Clone
            </button>
          ) : editMode ? (
            <>
              <button className="btn btn--primary" onClick={() => setEditorWidget(null)}>
                <Plus size={12} /> Add widget
              </button>
              <button className="xp-live-btn" onClick={() => { setEditMode(false); load(); }} title="Discard local widget changes">
                <X size={12} /> Cancel
              </button>
              <button className="btn btn--primary" onClick={() => void save(doc)} disabled={saving}>
                <Save size={12} /> {saving ? "Saving…" : hasUnsavedChanges ? "Save changes" : "Save"}
              </button>
            </>
          ) : (
            <button className="xp-live-btn" onClick={() => setEditMode(true)}>
              <Pencil size={12} /> Edit
            </button>
          )}
        </div>
      </div>

      {(doc.variables ?? []).length > 0 && (
        <DashboardVariableBar
          variables={doc.variables!}
          tenant={config.default_tenant}
          values={variableValues}
          onChange={handleVariableChange}
        />
      )}

      <div className="panel-body dashboard-view">
        {layout.length === 0 ? (
          <div className="xp-empty">
            This dashboard has no widgets yet.
            {!doc.builtIn && (
              <button className="btn btn--primary" onClick={() => { setEditMode(true); setEditorWidget(null); }}>
                <Plus size={12} /> Add the first widget
              </button>
            )}
          </div>
        ) : (
          <div
            className="dash-grid"
            style={{
              display: "grid",
              gridTemplateColumns: "repeat(12, 1fr)",
              gridAutoRows: "60px",
              gap: "0.5rem",
              gridTemplateRows: `repeat(${maxRow}, 60px)`,
            }}
          >
            {layout.map((widget) => (
              <DashboardWidgetCard
                key={widget.id}
                widget={widget}
                tenant={config.default_tenant}
                variableValues={variableValues}
                editMode={editMode && !doc.builtIn}
                onEdit={() => setEditorWidget(widget)}
                onRemove={() => removeWidget(widget.id)}
              />
            ))}
          </div>
        )}
      </div>

      {editorWidget !== undefined && (
        <WidgetEditorModal
          widget={editorWidget ?? undefined}
          onSave={saveWidget}
          onClose={() => setEditorWidget(undefined)}
        />
      )}
    </div>
  );
}

function slugify(value: string): string {
  return value
    .toLowerCase()
    .trim()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 64);
}
