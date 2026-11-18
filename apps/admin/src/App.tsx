import React, { useCallback, useEffect, useRef, useState } from "react";
import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";
import {
  Activity,
  Ban,
  Check,
  Copy,
  Cpu,
  FolderOpen,
  KeyRound,
  Monitor,
  Play,
  Radio,
  Search,
  Shield,
  Sparkles,
  Trash2,
  User,
  Wifi,
} from "lucide-react";
import { GenerateClientModal } from "./components/GenerateClientModal";
import { RemoteViewer } from "./components/RemoteViewer";
import type { AgentSummary, ServerInfoResponse } from "./types";

export default function App() {
  const [serverInfo, setServerInfo] = useState<ServerInfoResponse | null>(null);
  const [agents, setAgents] = useState<AgentSummary[]>([]);
  const [search, setSearch] = useState("");
  const [activeTab, setActiveTab] = useState<"devices" | "tokens">("devices");
  const [showGenerator, setShowGenerator] = useState(false);
  const [controllingAgentId, setControllingAgentId] = useState<string | null>(
    null
  );
  const [copiedFp, setCopiedFp] = useState(false);
  const searchInputRef = useRef<HTMLInputElement>(null);

  const refreshServerInfo = useCallback(async () => {
    try {
      const info = await invoke<ServerInfoResponse>("get_server_info");
      setServerInfo(info);
    } catch (err) {
      console.error("Failed to fetch server info:", err);
    }
  }, []);

  const refreshAgents = useCallback(async () => {
    try {
      const list = await invoke<AgentSummary[]>("list_agents");
      setAgents(list);
    } catch (err) {
      console.error("Failed to fetch agents:", err);
    }
  }, []);

  useEffect(() => {
    refreshServerInfo();
    refreshAgents();

    const unlistenPromise = listen<AgentSummary[]>("agents-updated", (ev) => {
      setAgents(ev.payload);
    });

    const onKeyDown = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        searchInputRef.current?.focus();
      }
    };
    window.addEventListener("keydown", onKeyDown);

    return () => {
      window.removeEventListener("keydown", onKeyDown);
      unlistenPromise.then((fn) => fn());
    };
  }, [refreshAgents, refreshServerInfo]);

  const controlledAgent =
    agents.find((a) => a.agentId === controllingAgentId) ?? null;

  const filteredAgents = agents.filter((a) => {
    if (!search.trim()) return true;
    const q = search.toLowerCase();
    return (
      a.hostname.toLowerCase().includes(q) ||
      a.osUser.toLowerCase().includes(q) ||
      a.label.toLowerCase().includes(q) ||
      a.remoteAddr.toLowerCase().includes(q)
    );
  });

  const copyFingerprint = () => {
    if (!serverInfo) return;
    navigator.clipboard.writeText(serverInfo.certFingerprint);
    setCopiedFp(true);
    setTimeout(() => setCopiedFp(false), 1800);
  };

  const handleRevokeToken = async (tokenId: string) => {
    await invoke("revoke_token", { tokenId });
    await refreshServerInfo();
  };

  const handleDisconnectAgent = async (agentId: string) => {
    await invoke("disconnect_agent", { agentId });
  };

  return (
    <div className="flex h-screen w-screen overflow-hidden bg-[#090b10] text-zinc-100">
      {/* Sidebar */}
      <aside className="flex w-68 shrink-0 flex-col border-r border-zinc-800/80 bg-[#0c0f17]">
        {/* Brand */}
        <div className="flex items-center gap-3 px-5 py-5 border-b border-zinc-800/60">
          <div className="flex h-9 w-9 items-center justify-center rounded-xl bg-linear-to-br from-emerald-400 to-teal-600 text-zinc-950 font-black shadow-lg shadow-emerald-500/20">
            Z
          </div>
          <div>
            <div className="flex items-center gap-1.5">
              <h1 className="text-sm font-bold tracking-tight text-zinc-100">
                ZeroRMM
              </h1>
              <span className="rounded bg-emerald-500/15 px-1.5 py-0.5 font-mono text-[10px] font-semibold text-emerald-400">
                QUIC
              </span>
            </div>
            <p className="text-[11px] text-zinc-500">
              Reverse Zero-Copy Desktop
            </p>
          </div>
        </div>

        {/* Primary Action */}
        <div className="p-4">
          <button
            onClick={() => {
              refreshServerInfo();
              setShowGenerator(true);
            }}
            className="w-full inline-flex items-center justify-center gap-2 rounded-xl bg-emerald-500 px-4 py-2.5 text-xs font-semibold text-zinc-950 shadow-lg shadow-emerald-500/20 hover:bg-emerald-400 active:scale-[0.99] transition cursor-pointer"
          >
            <Sparkles className="h-4 w-4" />
            Generar Cliente
          </button>
        </div>

        {/* Navigation */}
        <nav className="flex-1 px-3 space-y-1">
          <button
            onClick={() => setActiveTab("devices")}
            className={`w-full flex items-center justify-between rounded-xl px-3 py-2.5 text-xs font-medium transition ${
              activeTab === "devices"
                ? "bg-zinc-800/90 text-zinc-100"
                : "text-zinc-400 hover:bg-zinc-900 hover:text-zinc-200"
            }`}
          >
            <span className="flex items-center gap-2.5">
              <Monitor className="h-4 w-4 text-emerald-400" />
              Equipos Online
            </span>
            <span className="rounded-md bg-emerald-500/15 px-2 py-0.5 font-mono text-[11px] text-emerald-300">
              {agents.length}
            </span>
          </button>

          <button
            onClick={() => {
              refreshServerInfo();
              setActiveTab("tokens");
            }}
            className={`w-full flex items-center justify-between rounded-xl px-3 py-2.5 text-xs font-medium transition ${
              activeTab === "tokens"
                ? "bg-zinc-800/90 text-zinc-100"
                : "text-zinc-400 hover:bg-zinc-900 hover:text-zinc-200"
            }`}
          >
            <span className="flex items-center gap-2.5">
              <KeyRound className="h-4 w-4 text-sky-400" />
              Clientes Emitidos
            </span>
            <span className="rounded-md bg-zinc-800 px-2 py-0.5 font-mono text-[11px] text-zinc-400">
              {serverInfo?.issuedTokens.length ?? 0}
            </span>
          </button>
        </nav>

        {/* Embedded Server Status Card */}
        <div className="m-3 rounded-xl border border-zinc-800/80 bg-[#090b10] p-3.5 space-y-2.5">
          <div className="flex items-center justify-between">
            <span className="flex items-center gap-1.5 text-[11px] font-semibold text-zinc-300">
              <Radio className="h-3.5 w-3.5 text-emerald-400 animate-pulse" />
              Backend Embebido
            </span>
            <span className="rounded-full bg-emerald-500/15 px-2 py-0.5 font-mono text-[10px] text-emerald-400">
              UDP :{serverInfo?.listenPort ?? 4433}
            </span>
          </div>

          <div className="space-y-1 text-[11px] font-mono text-zinc-400">
            <div className="flex items-center justify-between">
              <span className="text-zinc-500">IP LAN</span>
              <span className="text-zinc-200">
                {serverInfo?.localIps[0] ?? "—"}
              </span>
            </div>
            <div className="flex items-center justify-between">
              <span className="text-zinc-500">Stub</span>
              <span
                className={
                  serverInfo?.stubStatus.found
                    ? "text-emerald-400"
                    : "text-amber-400"
                }
              >
                {serverInfo?.stubStatus.found ? "Listo" : "Sin compilar"}
              </span>
            </div>
          </div>

          <button
            onClick={copyFingerprint}
            className="w-full flex items-center justify-between rounded-lg border border-zinc-800 bg-zinc-900/70 px-2.5 py-1.5 text-[10px] font-mono text-zinc-400 hover:border-zinc-700 hover:text-zinc-200 transition"
            title="Copiar huella SHA-256 TLS Pinning"
          >
            <span className="flex items-center gap-1.5 truncate">
              <Shield className="h-3 w-3 text-emerald-400 shrink-0" />
              {serverInfo
                ? `${serverInfo.certFingerprint.slice(0, 14)}…`
                : "..."}
            </span>
            {copiedFp ? (
              <Check className="h-3 w-3 text-emerald-400" />
            ) : (
              <Copy className="h-3 w-3" />
            )}
          </button>
        </div>
      </aside>

      {/* Main Content */}
      <main className="flex flex-1 flex-col min-w-0">
        {/* Top Bar */}
        <header className="flex items-center justify-between border-b border-zinc-800/80 px-6 py-4 bg-[#090b10]/80 backdrop-blur">
          <div>
            <h2 className="text-base font-semibold text-zinc-100">
              {activeTab === "devices"
                ? "Equipos Conectados (Reverse Phone Home)"
                : "Clientes Generados y Control de Acceso"}
            </h2>
            <p className="text-xs text-zinc-500 mt-0.5">
              {activeTab === "devices"
                ? "Haz doble clic sobre cualquier equipo Online para iniciar control remoto Zero-Copy."
                : "Cada ejecutable lleva un token Ed25519 único revocable de forma instantánea."}
            </p>
          </div>

          {activeTab === "devices" && (
            <div className="relative w-72">
              <Search className="pointer-events-none absolute left-3 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-zinc-500" />
              <input
                ref={searchInputRef}
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                placeholder="Buscar equipo, usuario o IP… (Ctrl+K)"
                className="w-full rounded-xl border border-zinc-800 bg-[#10131c] pl-9 pr-3 py-2 text-xs text-zinc-200 placeholder:text-zinc-500 focus:border-emerald-500/60 focus:outline-none"
              />
            </div>
          )}
        </header>

        {/* Body */}
        <div className="flex-1 overflow-y-auto p-6">
          {activeTab === "devices" ? (
            filteredAgents.length === 0 ? (
              <div className="flex h-full flex-col items-center justify-center text-center max-w-md mx-auto">
                <div className="flex h-16 w-16 items-center justify-center rounded-2xl border border-zinc-800 bg-[#10131c] text-emerald-400 mb-4 shadow-xl">
                  <Radio className="h-7 w-7 opacity-80" />
                </div>
                <h3 className="text-base font-semibold text-zinc-100">
                  Esperando conexiones inversas en UDP :
                  {serverInfo?.listenPort ?? 4433}
                </h3>
                <p className="mt-2 text-xs leading-relaxed text-zinc-400">
                  Pulsa <strong className="text-zinc-200">Generar Cliente</strong>{" "}
                  para emitir un ejecutable ligero de un solo archivo. En cuanto el usuario haga doble clic, su equipo aparecerá aquí instantáneamente.
                </p>
                <button
                  onClick={() => {
                    refreshServerInfo();
                    setShowGenerator(true);
                  }}
                  className="mt-5 inline-flex items-center gap-2 rounded-xl border border-emerald-500/40 bg-emerald-500/10 px-4 py-2 text-xs font-semibold text-emerald-300 hover:bg-emerald-500/20 transition"
                >
                  <Sparkles className="h-4 w-4" />
                  Generar primer cliente
                </button>
              </div>
            ) : (
              <div className="grid grid-cols-1 lg:grid-cols-2 xl:grid-cols-3 gap-4">
                {filteredAgents.map((agent) => (
                  <div
                    key={agent.agentId}
                    onDoubleClick={() => setControllingAgentId(agent.agentId)}
                    className="group relative flex flex-col justify-between rounded-2xl border border-zinc-800/90 bg-[#10131c] p-5 hover:border-emerald-500/50 hover:bg-[#131724] transition cursor-pointer shadow-lg"
                  >
                    <div>
                      <div className="flex items-start justify-between gap-2">
                        <div className="flex items-center gap-3 min-w-0">
                          <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-emerald-500/10 border border-emerald-500/25 text-emerald-400">
                            <Monitor className="h-5 w-5" />
                          </div>
                          <div className="min-w-0">
                            <div className="flex items-center gap-2">
                              <h3 className="text-sm font-semibold text-zinc-100 truncate">
                                {agent.hostname}
                              </h3>
                              <span className="inline-flex items-center gap-1 rounded-full bg-emerald-500/15 px-2 py-0.5 text-[10px] font-semibold text-emerald-400">
                                <span className="h-1.5 w-1.5 rounded-full bg-emerald-400 animate-pulse" />
                                Online
                              </span>
                            </div>
                            <div className="mt-0.5 flex items-center gap-1.5 text-xs text-zinc-400">
                              <User className="h-3 w-3 text-zinc-500" />
                              <span className="truncate">{agent.osUser}</span>
                              <span className="text-zinc-600">•</span>
                              <span className="truncate text-emerald-300/80">
                                {agent.label}
                              </span>
                            </div>
                          </div>
                        </div>

                        <button
                          onClick={(e) => {
                            e.stopPropagation();
                            handleDisconnectAgent(agent.agentId);
                          }}
                          title="Desconectar agente"
                          className="opacity-0 group-hover:opacity-100 rounded-lg p-1.5 text-zinc-500 hover:bg-red-500/15 hover:text-red-400 transition"
                        >
                          <Trash2 className="h-3.5 w-3.5" />
                        </button>
                      </div>

                      {/* Hardware & Network metadata */}
                      <div className="mt-4 grid grid-cols-2 gap-2 rounded-xl bg-[#090b10]/80 border border-zinc-800/70 p-3 text-[11px] font-mono">
                        <div className="flex items-center gap-1.5 text-zinc-400 truncate">
                          <Cpu className="h-3.5 w-3.5 text-sky-400 shrink-0" />
                          <span className="truncate" title={agent.gpuName}>
                            {agent.gpuName}
                          </span>
                        </div>
                        <div className="flex items-center justify-end gap-1.5 text-zinc-300">
                          <Wifi className="h-3.5 w-3.5 text-emerald-400" />
                          <span>{agent.rttMs} ms</span>
                        </div>
                        <div className="text-zinc-500 truncate">
                          {agent.remoteAddr}
                        </div>
                        <div className="text-right text-zinc-400">
                          {agent.displays.length}{" "}
                          {agent.displays.length === 1 ? "monitor" : "monitores"}
                        </div>
                      </div>
                    </div>

                    {/* Footer CTA */}
                    <div className="mt-4 flex items-center justify-between pt-3 border-t border-zinc-800/60">
                      <span className="text-[11px] text-zinc-500 flex items-center gap-1.5">
                        <Activity className="h-3.5 w-3.5 text-zinc-600" />
                        {agent.streaming
                          ? `En vivo · ${Math.round(agent.telemetry.fps)} FPS`
                          : "Doble clic para controlar"}
                      </span>
                      <button
                        onClick={(e) => {
                          e.stopPropagation();
                          setControllingAgentId(agent.agentId);
                        }}
                        className="inline-flex items-center gap-1.5 rounded-lg bg-emerald-500/15 border border-emerald-500/30 px-3 py-1.5 text-xs font-semibold text-emerald-300 hover:bg-emerald-500 hover:text-zinc-950 transition"
                      >
                        <Play className="h-3 w-3 fill-current" />
                        Controlar
                      </button>
                    </div>
                  </div>
                ))}
              </div>
            )
          ) : (
            /* Issued Tokens Tab */
            <div className="rounded-2xl border border-zinc-800 bg-[#10131c] overflow-hidden">
              <table className="w-full text-left text-xs">
                <thead className="border-b border-zinc-800 bg-[#0c0f17] text-[11px] uppercase tracking-wider text-zinc-500">
                  <tr>
                    <th className="px-5 py-3">Etiqueta / Token</th>
                    <th className="px-5 py-3">Endpoints</th>
                    <th className="px-5 py-3">Estado</th>
                    <th className="px-5 py-3 text-right">Acciones</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-zinc-800/70">
                  {(serverInfo?.issuedTokens ?? []).length === 0 ? (
                    <tr>
                      <td
                        colSpan={4}
                        className="px-5 py-12 text-center text-zinc-500"
                      >
                        Aún no has generado ningún ejecutable cliente.
                      </td>
                    </tr>
                  ) : (
                    serverInfo?.issuedTokens.map((t) => (
                      <tr key={t.tokenId} className="hover:bg-zinc-900/40">
                        <td className="px-5 py-3.5">
                          <div className="font-medium text-zinc-200">
                            {t.label}
                          </div>
                          <div className="font-mono text-[11px] text-zinc-500">
                            {t.tokenId}
                          </div>
                        </td>
                        <td className="px-5 py-3.5 font-mono text-[11px] text-zinc-400">
                          {t.endpoints.join(", ")}
                        </td>
                        <td className="px-5 py-3.5">
                          {t.revoked ? (
                            <span className="inline-flex items-center gap-1 rounded-full bg-red-500/15 px-2.5 py-0.5 text-[11px] font-medium text-red-400">
                              Revocado
                            </span>
                          ) : (
                            <span className="inline-flex items-center gap-1 rounded-full bg-emerald-500/15 px-2.5 py-0.5 text-[11px] font-medium text-emerald-400">
                              Activo
                            </span>
                          )}
                        </td>
                        <td className="px-5 py-3.5 text-right space-x-2">
                          <button
                            onClick={() =>
                              invoke("reveal_in_explorer", {
                                path: t.outputPath,
                              })
                            }
                            className="inline-flex items-center gap-1 rounded-lg border border-zinc-800 px-2.5 py-1 text-[11px] text-zinc-300 hover:bg-zinc-800 transition"
                          >
                            <FolderOpen className="h-3 w-3" />
                            Archivo
                          </button>
                          {!t.revoked && (
                            <button
                              onClick={() => handleRevokeToken(t.tokenId)}
                              className="inline-flex items-center gap-1 rounded-lg border border-red-500/30 bg-red-500/10 px-2.5 py-1 text-[11px] text-red-300 hover:bg-red-500/20 transition"
                            >
                              <Ban className="h-3 w-3" />
                              Revocar
                            </button>
                          )}
                        </td>
                      </tr>
                    ))
                  )}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </main>

      {/* Modals & Full-Screen Viewer */}
      {showGenerator && serverInfo && (
        <GenerateClientModal
          serverInfo={serverInfo}
          onClose={() => setShowGenerator(false)}
          onGenerated={refreshServerInfo}
        />
      )}

      {controlledAgent && (
        <RemoteViewer
          agent={controlledAgent}
          onClose={() => setControllingAgentId(null)}
        />
      )}
    </div>
  );
}
