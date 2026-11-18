import React, { useCallback, useEffect, useRef, useState } from "react";
import { Channel, invoke } from "@tauri-apps/api/core";
import {
  Activity,
  Cpu,
  Gauge,
  Maximize2,
  Minimize2,
  Monitor,
  Power,
  RefreshCw,
  ShieldCheck,
  Wifi,
} from "lucide-react";
import { CODE_TO_SCANCODE, mouseButtonName } from "../scancodes";
import type { AgentSummary } from "../types";

interface Props {
  agent: AgentSummary;
  onClose: () => void;
}

const FLAG_KEYFRAME = 1 << 0;
const FLAG_HEVC = 1 << 1;

/** Extract exact H.264 avc1.PPCCLL codec string from an Annex-B SPS NALU (nal_type = 7). */
function detectH264CodecString(annexb: Uint8Array): string {
  for (let i = 0; i + 6 < annexb.length; i++) {
    let scLen = 0;
    if (
      annexb[i] === 0 &&
      annexb[i + 1] === 0 &&
      annexb[i + 2] === 0 &&
      annexb[i + 3] === 1
    ) {
      scLen = 4;
    } else if (annexb[i] === 0 && annexb[i + 1] === 0 && annexb[i + 2] === 1) {
      scLen = 3;
    }
    if (scLen > 0) {
      const nalType = annexb[i + scLen] & 0x1f;
      if (nalType === 7 && i + scLen + 3 < annexb.length) {
        const p = annexb[i + scLen + 1].toString(16).padStart(2, "0");
        const c = annexb[i + scLen + 2].toString(16).padStart(2, "0");
        const l = annexb[i + scLen + 3].toString(16).padStart(2, "0");
        return `avc1.${p}${c}${l}`;
      }
    }
  }
  return "avc1.4d002a"; // Main Profile Level 4.2 safe fallback
}

export const RemoteViewer: React.FC<Props> = ({ agent, onClose }) => {
  const containerRef = useRef<HTMLDivElement>(null);
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const decoderRef = useRef<VideoDecoder | null>(null);
  const decoderConfiguredRef = useRef(false);
  const moveSeqRef = useRef(0);
  const lastMoveSentRef = useRef(0);
  const renderedFramesRef = useRef(0);

  const [displayIndex, setDisplayIndex] = useState(
    agent.displays.find((d) => d.is_primary)?.index ?? 0
  );
  const [targetFps, setTargetFps] = useState(60);
  const [bitrateKbps, setBitrateKbps] = useState(12000);
  const [showHud, setShowHud] = useState(true);
  const [isFullscreen, setIsFullscreen] = useState(false);
  const [localFps, setLocalFps] = useState(0);
  const [videoDims, setVideoDims] = useState({ w: 0, h: 0 });
  const [statusText, setStatusText] = useState("Negociando hardware encoder...");

  const stopSession = useCallback(async () => {
    try {
      await invoke("stop_remote_session", { agentId: agent.agentId });
    } catch {
      // ignore on disconnect
    }
  }, [agent.agentId]);

  // Measure real local rendered FPS
  useEffect(() => {
    const id = setInterval(() => {
      setLocalFps(renderedFramesRef.current);
      renderedFramesRef.current = 0;
    }, 1000);
    return () => clearInterval(id);
  }, []);

  // Initialize WebCodecs Hardware VideoDecoder + Binary Tauri Channel
  useEffect(() => {
    let active = true;
    decoderConfiguredRef.current = false;

    const canvas = canvasRef.current;
    const ctx = canvas?.getContext("2d", {
      alpha: false,
      desynchronized: true,
    });

    if (typeof VideoDecoder === "undefined") {
      setStatusText("WebCodecs API no disponible en este WebView");
      return;
    }

    const decoder = new VideoDecoder({
      output: (frame: VideoFrame) => {
        if (!active || !canvas || !ctx) {
          frame.close();
          return;
        }
        if (
          canvas.width !== frame.displayWidth ||
          canvas.height !== frame.displayHeight
        ) {
          canvas.width = frame.displayWidth;
          canvas.height = frame.displayHeight;
          setVideoDims({ w: frame.displayWidth, h: frame.displayHeight });
        }
        ctx.drawImage(frame, 0, 0);
        frame.close();
        renderedFramesRef.current += 1;
      },
      error: (err) => {
        console.error("WebCodecs VideoDecoder error:", err);
        decoderConfiguredRef.current = false;
        invoke("request_idr", { agentId: agent.agentId }).catch(() => {});
      },
    });
    decoderRef.current = decoder;

    const channel = new Channel<ArrayBuffer>();
    channel.onmessage = (rawBuf: ArrayBuffer) => {
      if (!active || !decoderRef.current) return;
      const bytes = new Uint8Array(rawBuf);
      if (bytes.byteLength <= 12) return;

      const view = new DataView(rawBuf);
      const flags = view.getUint8(0);
      const tsUs = view.getUint32(4, true);
      const payload = bytes.subarray(12);
      const isKeyframe = (flags & FLAG_KEYFRAME) !== 0;
      const isHevc = (flags & FLAG_HEVC) !== 0;

      if (!decoderConfiguredRef.current) {
        if (!isKeyframe) {
          return; // Wait for first IDR carrying SPS/PPS
        }
        const codecStr = isHevc
          ? "hvc1.1.6.L120.90"
          : detectH264CodecString(payload);
        try {
          decoder.configure({
            codec: codecStr,
            hardwareAcceleration: "prefer-hardware",
            optimizeForLatency: true,
          });
          decoderConfiguredRef.current = true;
          setStatusText("");
        } catch (err) {
          console.error("Decoder configure error:", err);
          setStatusText(`Error de decodificación: ${String(err)}`);
          return;
        }
      }

      // Drop-on-backpressure: if WebCodecs decode queue builds up, wait for next IDR
      if (decoder.decodeQueueSize > 3 && !isKeyframe) {
        return;
      }

      try {
        const chunk = new EncodedVideoChunk({
          type: isKeyframe ? "key" : "delta",
          timestamp: tsUs,
          data: payload,
        });
        decoder.decode(chunk);
      } catch (err) {
        console.warn("Decode chunk failed, requesting IDR:", err);
        invoke("request_idr", { agentId: agent.agentId }).catch(() => {});
      }
    };

    invoke("start_remote_session", {
      req: {
        agentId: agent.agentId,
        displayIndex,
        targetFps,
        bitrateKbps,
        codec: "h264",
        maxWidth: 0,
      },
      videoChannel: channel,
    }).catch((err) => {
      setStatusText(`No se pudo iniciar stream: ${String(err)}`);
    });

    return () => {
      active = false;
      stopSession();
      try {
        decoder.close();
      } catch {
        // already closed
      }
      decoderRef.current = null;
    };
  }, [agent.agentId, displayIndex]);

  // Dynamic bitrate/fps adjustment without restarting stream
  const handleQualityChange = (newBitrate: number, newFps: number) => {
    setBitrateKbps(newBitrate);
    setTargetFps(newFps);
    invoke("adjust_encoder", {
      agentId: agent.agentId,
      bitrateKbps: newBitrate,
      targetFps: newFps,
    }).catch(() => {});
  };

  // Normalize mouse coordinates inside the rendered video canvas (0..65535)
  const getNormalizedCoords = (e: React.PointerEvent<HTMLCanvasElement>) => {
    const canvas = canvasRef.current;
    if (!canvas) return null;
    const rect = canvas.getBoundingClientRect();
    if (rect.width <= 0 || rect.height <= 0) return null;
    const nx = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
    const ny = Math.max(0, Math.min(1, (e.clientY - rect.top) / rect.height));
    return {
      x: Math.round(nx * 65535),
      y: Math.round(ny * 65535),
    };
  };

  const handlePointerMove = (e: React.PointerEvent<HTMLCanvasElement>) => {
    const now = performance.now();
    // Coalesce mouse moves up to ~125Hz for ultra-smooth datagrams without flooding IPC
    if (now - lastMoveSentRef.current < 8) return;
    lastMoveSentRef.current = now;

    const coords = getNormalizedCoords(e);
    if (!coords) return;
    moveSeqRef.current = (moveSeqRef.current + 1) & 0xffff;
    invoke("send_mouse_move", {
      agentId: agent.agentId,
      displayIndex,
      x: coords.x,
      y: coords.y,
      seq: moveSeqRef.current,
    }).catch(() => {});
  };

  const handlePointerDown = (e: React.PointerEvent<HTMLCanvasElement>) => {
    e.preventDefault();
    canvasRef.current?.focus();
    const coords = getNormalizedCoords(e);
    if (!coords) return;
    invoke("send_critical_input", {
      agentId: agent.agentId,
      event: {
        kind: "mouseButton",
        button: mouseButtonName(e.button),
        pressed: true,
        x: coords.x,
        y: coords.y,
      },
    }).catch(() => {});
  };

  const handlePointerUp = (e: React.PointerEvent<HTMLCanvasElement>) => {
    e.preventDefault();
    const coords = getNormalizedCoords(e);
    if (!coords) return;
    invoke("send_critical_input", {
      agentId: agent.agentId,
      event: {
        kind: "mouseButton",
        button: mouseButtonName(e.button),
        pressed: false,
        x: coords.x,
        y: coords.y,
      },
    }).catch(() => {});
  };

  const handleWheel = (e: React.WheelEvent<HTMLCanvasElement>) => {
    e.preventDefault();
    const deltaY = Math.round(-Math.sign(e.deltaY) * 120);
    const deltaX = Math.round(Math.sign(e.deltaX) * 120);
    invoke("send_critical_input", {
      agentId: agent.agentId,
      event: { kind: "mouseWheel", deltaX, deltaY },
    }).catch(() => {});
  };

  const handleKey = (e: React.KeyboardEvent, pressed: boolean) => {
    const mapping = CODE_TO_SCANCODE[e.code];
    if (!mapping) return;
    e.preventDefault();
    e.stopPropagation();
    const [scancode, extended] = mapping;
    invoke("send_critical_input", {
      agentId: agent.agentId,
      event: { kind: "key", scancode, extended, pressed },
    }).catch(() => {});
  };

  const handleBlur = () => {
    invoke("send_critical_input", {
      agentId: agent.agentId,
      event: { kind: "releaseAll" },
    }).catch(() => {});
  };

  const toggleFullscreen = async () => {
    if (!document.fullscreenElement) {
      await containerRef.current?.requestFullscreen();
      setIsFullscreen(true);
    } else {
      await document.exitFullscreen();
      setIsFullscreen(false);
    }
  };

  return (
    <div
      ref={containerRef}
      className="fixed inset-0 z-40 flex flex-col bg-[#050609] select-none"
    >
      {/* Floating Glassmorphism Control Toolbar */}
      <div className="pointer-events-none absolute top-3 left-0 right-0 z-50 flex justify-center px-4">
        <div className="pointer-events-auto flex items-center gap-2 rounded-2xl border border-zinc-700/70 bg-[#0d111a]/85 px-3.5 py-2 shadow-2xl backdrop-blur-xl">
          {/* Host badge */}
          <div className="flex items-center gap-2 pr-2 border-r border-zinc-800">
            <span className="h-2 w-2 rounded-full bg-emerald-400 shadow-[0_0_8px_#34d399]" />
            <span className="text-xs font-semibold text-zinc-100">
              {agent.hostname}
            </span>
            <span className="text-[11px] text-zinc-400">
              ({agent.osUser})
            </span>
          </div>

          {/* Hardware encoder pill */}
          <div className="hidden md:flex items-center gap-1.5 rounded-lg bg-zinc-900/90 border border-zinc-800 px-2 py-1 text-[11px] font-mono">
            <Cpu
              className={`h-3.5 w-3.5 ${
                agent.isHardwareEncoder ? "text-emerald-400" : "text-amber-400"
              }`}
            />
            <span className="text-zinc-300 max-w-40 truncate">
              {agent.encoderName ?? "Inicializando..."}
            </span>
          </div>

          {/* Display Switcher */}
          {agent.displays.length > 1 && (
            <div className="flex items-center gap-1 border-l border-zinc-800 pl-2">
              <Monitor className="h-3.5 w-3.5 text-zinc-400" />
              {agent.displays.map((d) => (
                <button
                  key={d.index}
                  onClick={() => setDisplayIndex(d.index)}
                  className={`rounded-md px-2 py-0.5 text-[11px] font-mono transition ${
                    displayIndex === d.index
                      ? "bg-emerald-500 text-zinc-950 font-semibold"
                      : "bg-zinc-900 text-zinc-400 hover:text-zinc-100"
                  }`}
                >
                  #{d.index + 1}
                </button>
              ))}
            </div>
          )}

          {/* Quality / Bitrate */}
          <select
            value={bitrateKbps}
            onChange={(e) => handleQualityChange(Number(e.target.value), targetFps)}
            className="rounded-lg border border-zinc-800 bg-zinc-900 px-2 py-1 text-[11px] font-mono text-zinc-300 focus:outline-none"
            title="Bitrate del Hardware Encoder"
          >
            <option value={4000}>4 Mbps</option>
            <option value={8000}>8 Mbps</option>
            <option value={12000}>12 Mbps</option>
            <option value={20000}>20 Mbps</option>
            <option value={35000}>35 Mbps (Ultra)</option>
          </select>

          <select
            value={targetFps}
            onChange={(e) => handleQualityChange(bitrateKbps, Number(e.target.value))}
            className="rounded-lg border border-zinc-800 bg-zinc-900 px-2 py-1 text-[11px] font-mono text-zinc-300 focus:outline-none"
            title="FPS Objetivo"
          >
            <option value={30}>30 FPS</option>
            <option value={60}>60 FPS</option>
            <option value={120}>120 FPS</option>
          </select>

          {/* Force IDR */}
          <button
            onClick={() => invoke("request_idr", { agentId: agent.agentId })}
            title="Forzar Keyframe IDR"
            className="rounded-lg p-1.5 text-zinc-400 hover:bg-zinc-800 hover:text-zinc-100 transition"
          >
            <RefreshCw className="h-3.5 w-3.5" />
          </button>

          {/* Toggle HUD */}
          <button
            onClick={() => setShowHud((v) => !v)}
            title="Mostrar/Ocultar HUD de Telemetría"
            className={`rounded-lg p-1.5 transition ${
              showHud
                ? "bg-emerald-500/20 text-emerald-300"
                : "text-zinc-400 hover:bg-zinc-800 hover:text-zinc-100"
            }`}
          >
            <Activity className="h-3.5 w-3.5" />
          </button>

          {/* Fullscreen */}
          <button
            onClick={toggleFullscreen}
            title="Pantalla completa"
            className="rounded-lg p-1.5 text-zinc-400 hover:bg-zinc-800 hover:text-zinc-100 transition"
          >
            {isFullscreen ? (
              <Minimize2 className="h-3.5 w-3.5" />
            ) : (
              <Maximize2 className="h-3.5 w-3.5" />
            )}
          </button>

          {/* Close session */}
          <button
            onClick={async () => {
              await stopSession();
              onClose();
            }}
            className="ml-1 inline-flex items-center gap-1.5 rounded-lg bg-red-500/90 px-3 py-1.5 text-xs font-semibold text-white hover:bg-red-500 transition"
          >
            <Power className="h-3.5 w-3.5" />
            Finalizar
          </button>
        </div>
      </div>

      {/* Real-time Engineering Telemetry HUD */}
      {showHud && (
        <div className="pointer-events-none absolute bottom-4 left-4 z-50 rounded-xl border border-zinc-800/80 bg-[#090b10]/85 px-3.5 py-2.5 backdrop-blur-md font-mono text-[11px] text-zinc-300 shadow-xl">
          <div className="flex items-center gap-4">
            <div className="flex items-center gap-1.5">
              <Gauge className="h-3.5 w-3.5 text-emerald-400" />
              <span className="text-zinc-500">FPS</span>
              <span className="font-semibold text-emerald-300">
                {localFps || Math.round(agent.telemetry.fps)}
              </span>
            </div>
            <div className="flex items-center gap-1.5">
              <Wifi className="h-3.5 w-3.5 text-sky-400" />
              <span className="text-zinc-500">RTT</span>
              <span className="font-semibold text-sky-300">{agent.rttMs} ms</span>
            </div>
            <div>
              <span className="text-zinc-500">CAP </span>
              <span>{agent.telemetry.captureMs.toFixed(1)} ms</span>
            </div>
            <div>
              <span className="text-zinc-500">ENC </span>
              <span>{agent.telemetry.encodeMs.toFixed(1)} ms</span>
            </div>
            <div>
              <span className="text-zinc-500">RATE </span>
              <span>{(agent.telemetry.bitrateKbps / 1000).toFixed(1)} Mbps</span>
            </div>
            {videoDims.w > 0 && (
              <div className="text-zinc-400">
                {videoDims.w}×{videoDims.h}
              </div>
            )}
            <div className="flex items-center gap-1 text-emerald-400">
              <ShieldCheck className="h-3.5 w-3.5" />
              <span>QUIC TLS 1.3</span>
            </div>
          </div>
        </div>
      )}

      {/* Video Surface */}
      <div className="flex flex-1 items-center justify-center overflow-hidden p-2 pt-16">
        {statusText && (
          <div className="absolute flex flex-col items-center gap-3 text-center">
            <div className="h-8 w-8 animate-spin rounded-full border-2 border-emerald-500/30 border-t-emerald-400" />
            <p className="text-sm text-zinc-300">{statusText}</p>
            <p className="text-xs font-mono text-zinc-500">
              DXGI Zero-Copy → Media Foundation HW MFT → QUIC Datagrams
            </p>
          </div>
        )}
        <canvas
          ref={canvasRef}
          tabIndex={0}
          onContextMenu={(e) => e.preventDefault()}
          onPointerMove={handlePointerMove}
          onPointerDown={handlePointerDown}
          onPointerUp={handlePointerUp}
          onWheel={handleWheel}
          onKeyDown={(e) => handleKey(e, true)}
          onKeyUp={(e) => handleKey(e, false)}
          onBlur={handleBlur}
          className="max-h-full max-w-full rounded-lg shadow-2xl outline-none cursor-crosshair bg-black"
        />
      </div>
    </div>
  );
};
