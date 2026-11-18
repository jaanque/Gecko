import React, { useState } from "react";
import { invoke } from "@tauri-apps/api/core";
import {
  CheckCircle2,
  Cpu,
  FolderOpen,
  Lock,
  Plus,
  Sparkles,
  X,
  AlertTriangle,
} from "lucide-react";
import type { GenerateClientResponse, ServerInfoResponse } from "../types";

interface Props {
  serverInfo: ServerInfoResponse;
  onClose: () => void;
  onGenerated: () => void;
}

export const GenerateClientModal: React.FC<Props> = ({
  serverInfo,
  onClose,
  onGenerated,
}) => {
  const defaultEndpoint = `${serverInfo.localIps[0] ?? "127.0.0.1"}:${serverInfo.listenPort}`;
  const [label, setLabel] = useState("Soporte Express");
  const [adminName, setAdminName] = useState("Soporte IT");
  const [endpointsText, setEndpointsText] = useState(defaultEndpoint);
  const [ttlHours, setTtlHours] = useState<number>(24);
  const [fileName, setFileName] = useState("Gecko-Soporte.exe");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<GenerateClientResponse | null>(null);

  const handleGenerate = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const endpoints = endpointsText
        .split(/[\n,]+/)
        .map((s) => s.trim())
        .filter(Boolean);

      const res = await invoke<GenerateClientResponse>("generate_client", {
        req: {
          label,
          adminName,
          endpoints,
          ttlHours: ttlHours === 0 ? null : ttlHours,
          fileName,
        },
      });
      setResult(res);
      onGenerated();
    } catch (err) {
      setError(String(err));
    } finally {
      setBusy(false);
    }
  };

  const revealFile = async () => {
    if (!result) return;
    await invoke("reveal_in_explorer", { path: result.outputPath });
  };

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/75 backdrop-blur-md p-4">
      <div className="w-full max-w-xl rounded-2xl border border-zinc-800 bg-[#10131c] shadow-2xl overflow-hidden">
        {/* Header */}
        <div className="flex items-center justify-between border-b border-zinc-800/80 px-6 py-4">
          <div className="flex items-center gap-3">
            <div className="flex h-9 w-9 items-center justify-center rounded-xl bg-emerald-500/15 border border-emerald-500/30 text-emerald-400">
              <Sparkles className="h-4 w-4" />
            </div>
            <div>
              <h2 className="text-base font-semibold text-zinc-100">
                Generar Cliente Zero-Config
              </h2>
              <p className="text-xs text-zinc-400">
                Inyección instantánea de overlay firmado Ed25519 + TLS Pinning
              </p>
            </div>
          </div>
          <button
            onClick={onClose}
            className="rounded-lg p-1.5 text-zinc-400 hover:bg-zinc-800 hover:text-zinc-100 transition"
          >
            <X className="h-4 w-4" />
          </button>
        </div>

        {/* Body */}
        <div className="p-6">
          {!serverInfo.stubStatus.found && (
            <div className="mb-4 flex items-start gap-3 rounded-xl border border-amber-500/30 bg-amber-500/10 p-3.5 text-xs text-amber-200">
              <AlertTriangle className="h-4 w-4 shrink-0 text-amber-400 mt-0.5" />
              <div>
                <p className="font-medium text-amber-300">
                  Plantilla precompilada (rmm-agent.exe) no encontrada
                </p>
                <p className="mt-1 text-amber-200/80">
                  Compila primero el stub con{" "}
                  <code className="rounded bg-black/40 px-1.5 py-0.5 font-mono text-[11px]">
                    cargo build -p rmm-agent --profile release-agent
                  </code>{" "}
                  para habilitar la inyección instantánea.
                </p>
              </div>
            </div>
          )}

          {result ? (
            <div className="space-y-4">
              <div className="flex items-start gap-3 rounded-xl border border-emerald-500/30 bg-emerald-500/10 p-4">
                <CheckCircle2 className="h-5 w-5 text-emerald-400 shrink-0 mt-0.5" />
                <div className="min-w-0 flex-1">
                  <p className="text-sm font-semibold text-emerald-300">
                    Ejecutable listo para enviar
                  </p>
                  <p className="mt-1 text-xs text-zinc-300">
                    El usuario final solo debe hacer doble clic — sin IDs, sin contraseñas y sin instalación.
                  </p>
                  <div className="mt-3 rounded-lg bg-black/40 border border-zinc-800 p-2.5 font-mono text-[11px] text-zinc-300 break-all">
                    {result.outputPath}
                  </div>
                  <div className="mt-3 flex flex-wrap gap-3 text-[11px] text-zinc-400 font-mono">
                    <span>
                      Tamaño: {(result.sizeBytes / (1024 * 1024)).toFixed(2)} MB
                    </span>
                    <span>•</span>
                    <span>Token: {result.tokenId.slice(0, 8)}</span>
                  </div>
                </div>
              </div>

              <div className="flex items-center justify-end gap-2.5 pt-2">
                <button
                  onClick={() => setResult(null)}
                  className="rounded-lg border border-zinc-700 px-4 py-2 text-xs font-medium text-zinc-300 hover:bg-zinc-800 transition"
                >
                  Generar otro
                </button>
                <button
                  onClick={revealFile}
                  className="inline-flex items-center gap-2 rounded-lg bg-emerald-500 px-4 py-2 text-xs font-semibold text-zinc-950 hover:bg-emerald-400 transition"
                >
                  <FolderOpen className="h-4 w-4" />
                  Abrir en Explorador
                </button>
              </div>
            </div>
          ) : (
            <form onSubmit={handleGenerate} className="space-y-4">
              <div className="grid grid-cols-2 gap-3">
                <div>
                  <label className="block text-xs font-medium text-zinc-300 mb-1.5">
                    Etiqueta del Cliente
                  </label>
                  <input
                    value={label}
                    onChange={(e) => {
                      setLabel(e.target.value);
                      const clean = e.target.value
                        .replace(/[^a-zA-Z0-9-_]+/g, "-")
                        .replace(/^-|-$/g, "");
                      setFileName(`Gecko-${clean || "Client"}.exe`);
                    }}
                    placeholder="Ej: ACME - Contabilidad"
                    className="w-full rounded-lg border border-zinc-800 bg-[#090b10] px-3 py-2 text-sm text-zinc-100 placeholder:text-zinc-600 focus:border-emerald-500/60 focus:outline-none"
                    required
                  />
                </div>
                <div>
                  <label className="block text-xs font-medium text-zinc-300 mb-1.5">
                    Nombre del Administrador
                  </label>
                  <input
                    value={adminName}
                    onChange={(e) => setAdminName(e.target.value)}
                    placeholder="Visible en bandeja del cliente"
                    className="w-full rounded-lg border border-zinc-800 bg-[#090b10] px-3 py-2 text-sm text-zinc-100 placeholder:text-zinc-600 focus:border-emerald-500/60 focus:outline-none"
                    required
                  />
                </div>
              </div>

              <div>
                <div className="flex items-center justify-between mb-1.5">
                  <label className="text-xs font-medium text-zinc-300">
                    Endpoints de Conexión Inversa (IP/Dominio:Puerto UDP)
                  </label>
                  <span className="text-[11px] font-mono text-zinc-500">
                    1 por línea (failover automático)
                  </span>
                </div>
                <textarea
                  rows={2}
                  value={endpointsText}
                  onChange={(e) => setEndpointsText(e.target.value)}
                  className="w-full rounded-lg border border-zinc-800 bg-[#090b10] px-3 py-2 font-mono text-xs text-emerald-300 placeholder:text-zinc-600 focus:border-emerald-500/60 focus:outline-none"
                  required
                />
                <div className="mt-1.5 flex flex-wrap gap-1.5">
                  {serverInfo.localIps.map((ip) => {
                    const ep = `${ip}:${serverInfo.listenPort}`;
                    return (
                      <button
                        type="button"
                        key={ip}
                        onClick={() =>
                          setEndpointsText((prev) =>
                            prev.includes(ep) ? prev : `${prev.trim()}\n${ep}`.trim()
                          )
                        }
                        className="inline-flex items-center gap-1 rounded-md border border-zinc-800 bg-zinc-900/70 px-2 py-0.5 font-mono text-[11px] text-zinc-400 hover:border-emerald-500/40 hover:text-emerald-300 transition"
                      >
                        <Plus className="h-3 w-3" /> {ep}
                      </button>
                    );
                  })}
                </div>
              </div>

              <div className="grid grid-cols-2 gap-3">
                <div>
                  <label className="block text-xs font-medium text-zinc-300 mb-1.5">
                    Nombre del Ejecutable
                  </label>
                  <input
                    value={fileName}
                    onChange={(e) => setFileName(e.target.value)}
                    className="w-full rounded-lg border border-zinc-800 bg-[#090b10] px-3 py-2 font-mono text-xs text-zinc-200 focus:border-emerald-500/60 focus:outline-none"
                    required
                  />
                </div>
                <div>
                  <label className="block text-xs font-medium text-zinc-300 mb-1.5">
                    Caducidad del Token
                  </label>
                  <select
                    value={ttlHours}
                    onChange={(e) => setTtlHours(Number(e.target.value))}
                    className="w-full rounded-lg border border-zinc-800 bg-[#090b10] px-3 py-2 text-xs text-zinc-200 focus:border-emerald-500/60 focus:outline-none"
                  >
                    <option value={1}>1 hora (Soporte puntual)</option>
                    <option value={24}>24 horas</option>
                    <option value={168}>7 días</option>
                    <option value={0}>Sin caducidad (revocable manualmente)</option>
                  </select>
                </div>
              </div>

              {/* Crypto summary */}
              <div className="flex items-center justify-between rounded-xl border border-zinc-800/80 bg-zinc-950/60 px-3.5 py-2.5 text-[11px] text-zinc-400">
                <div className="flex items-center gap-2">
                  <Lock className="h-3.5 w-3.5 text-emerald-400" />
                  <span>TLS Pinning SHA-256:</span>
                  <code className="font-mono text-zinc-300">
                    {serverInfo.certFingerprint.slice(0, 16)}…
                  </code>
                </div>
                <div className="flex items-center gap-1.5">
                  <Cpu className="h-3.5 w-3.5 text-sky-400" />
                  <span>
                    Stub:{" "}
                    {serverInfo.stubStatus.found
                      ? `${(serverInfo.stubStatus.sizeBytes / (1024 * 1024)).toFixed(2)} MB`
                      : "Pendiente"}
                  </span>
                </div>
              </div>

              {error && (
                <div className="rounded-lg border border-red-500/30 bg-red-500/10 px-3 py-2 text-xs text-red-300">
                  {error}
                </div>
              )}

              <div className="flex items-center justify-end gap-2.5 pt-2">
                <button
                  type="button"
                  onClick={onClose}
                  className="rounded-lg border border-zinc-800 px-4 py-2 text-xs font-medium text-zinc-400 hover:bg-zinc-800/70 hover:text-zinc-200 transition"
                >
                  Cancelar
                </button>
                <button
                  type="submit"
                  disabled={busy || !serverInfo.stubStatus.found}
                  className="inline-flex items-center gap-2 rounded-lg bg-emerald-500 px-4 py-2 text-xs font-semibold text-zinc-950 hover:bg-emerald-400 disabled:opacity-40 disabled:cursor-not-allowed transition"
                >
                  <Sparkles className="h-3.5 w-3.5" />
                  {busy ? "Inyectando..." : "Generar .EXE"}
                </button>
              </div>
            </form>
          )}
        </div>
      </div>
    </div>
  );
};
