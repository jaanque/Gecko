export interface DisplayInfo {
  index: number;
  name: string;
  width: number;
  height: number;
  left: number;
  top: number;
  is_primary: boolean;
}

export interface TelemetrySnapshot {
  fps: number;
  captureMs: number;
  encodeMs: number;
  bitrateKbps: number;
  droppedFrames: number;
}

export interface AgentSummary {
  agentId: string;
  tokenId: string;
  label: string;
  hostname: string;
  osUser: string;
  osVersion: string;
  gpuName: string;
  remoteAddr: string;
  displays: DisplayInfo[];
  supportedCodecs: string[];
  onlineSince: string;
  rttMs: number;
  streaming: boolean;
  activeDisplay: number;
  streamWidth: number;
  streamHeight: number;
  encoderName: string | null;
  isHardwareEncoder: boolean;
  telemetry: TelemetrySnapshot;
}

export interface StubStatus {
  found: boolean;
  path: string | null;
  sizeBytes: number;
}

export interface IssuedTokenRecord {
  tokenId: string;
  label: string;
  endpoints: string[];
  createdAt: string;
  expiresAt: string | null;
  outputPath: string;
  revoked: boolean;
}

export interface ServerInfoResponse {
  listenPort: number;
  localIps: string[];
  certFingerprint: string;
  stubStatus: StubStatus;
  generatedDir: string;
  issuedTokens: IssuedTokenRecord[];
}

export interface GenerateClientResponse {
  tokenId: string;
  outputPath: string;
  sizeBytes: number;
  endpoints: string[];
  expiresAt: string | null;
}

export interface CursorShapeEvent {
  agentId: string;
  shapeId: string;
  width: number;
  height: number;
  hotspotX: number;
  hotspotY: number;
  rgbaB64: string;
}

export interface CursorPosEvent {
  agentId: string;
  visible: boolean;
  x: number;
  y: number;
}
