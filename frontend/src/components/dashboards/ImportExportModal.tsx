import { useRef, useState } from "react";
import { Upload, X } from "lucide-react";
import { api } from "../../api/client";
import { readFileAsText } from "../../lib/download";

interface Props {
  tenant: string;
  onClose: () => void;
  onImported: () => void;
}

export default function ImportExportModal({ tenant, onClose, onImported }: Props) {
  const fileRef = useRef<HTMLInputElement>(null);
  const [text, setText] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);

  const handleFile = async (file: File) => {
    try {
      const content = await readFileAsText(file);
      setText(content);
    } catch {
      setError("Could not read file.");
    }
  };

  const handleDrop = (e: React.DragEvent) => {
    e.preventDefault();
    const file = e.dataTransfer.files[0];
    if (file) handleFile(file);
  };

  const doImport = async () => {
    setError("");
    setLoading(true);
    try {
      // The backend accepts both JSON and YAML. Detect JSON so we send the
      // matching content type; YAML is forwarded as-is instead of being forced
      // through JSON.parse (which previously made the YAML dropzone useless).
      let contentType = "application/json";
      try {
        JSON.parse(text);
      } catch {
        contentType = "application/yaml";
      }
      await api.importDashboardsText(text, tenant, contentType);
      onImported();
      onClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Import failed.");
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()} style={{ maxWidth: 560 }}>
        <div className="modal__header">
          <span>Import Dashboard</span>
          <button onClick={onClose}><X size={14} /></button>
        </div>
        <div className="modal__body">
          {/* Drag-and-drop zone */}
          <div
            className="import-dropzone"
            onDrop={handleDrop}
            onDragOver={(e) => e.preventDefault()}
            onClick={() => fileRef.current?.click()}
          >
            <Upload size={20} />
            <span>Drag & drop a .json or .yaml file, or click to browse</span>
          </div>
          <input
            ref={fileRef}
            type="file"
            accept=".json,.yaml,.yml"
            style={{ display: "none" }}
            onChange={(e) => { const f = e.target.files?.[0]; if (f) handleFile(f); }}
          />

          {/* Paste area */}
          <textarea
            className="import-paste"
            placeholder="Or paste JSON / YAML here…"
            value={text}
            onChange={(e) => setText(e.target.value)}
            rows={8}
          />

          {error && <div className="import-error">{error}</div>}
        </div>
        <div className="modal__footer">
          <button className="btn" onClick={onClose}>Cancel</button>
          <button className="btn btn--primary" onClick={doImport} disabled={loading || !text.trim()}>
            {loading ? "Importing…" : "Import"}
          </button>
        </div>
      </div>
    </div>
  );
}
