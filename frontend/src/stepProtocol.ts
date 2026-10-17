// Defines STEP worker requests, transferable geometry, loading progress, and identity-free timing diagnostics.

export interface CadLoadProgress {
  stage: "download" | "parse" | "meshes" | "render";
  // Parse counts belong to this substep, never to overall model readiness.
  parseStep?: "geometry" | "occt-init" | "occt";
  // During download, decoded bytes and indexed records advance concurrently.
  decodedBytes?: number;
  indexedRecords?: number | null;
  completed: number | null;
  total: number | null;
}

export type CadProgress = (progress: CadLoadProgress) => void;

export interface StepMesh {
  positions: Float32Array<ArrayBuffer>;
  normals: Float32Array<ArrayBuffer>;
  indices: Uint16Array<ArrayBuffer> | Uint32Array<ArrayBuffer> | null;
  color: [number, number, number] | null;
  min: [number, number, number];
  max: [number, number, number];
  sphere: { center: [number, number, number]; radius: number };
}

// Durations are exclusive: download measures network waits; decompression
// excludes synchronous indexing callbacks. Direct parsing includes incremental
// indexing and excludes packing, including discarded
// packing when an unsupported representation later requires OCCT.
export interface StepImportTimings {
  directParseMs: number;
  occtInitMs: number;
  occtParseMs: number;
  packingMs: number;
}

export interface StepWorkerTimings extends StepImportTimings {
  downloadMs: number;
  decompressMs: number;
  startedAt: number;
  sentAt: number;
}

export interface StepRequest {
  taskId: string;
  path: string;
}

export type StepMessage =
  | { type: "progress"; progress: CadLoadProgress }
  | { type: "result"; meshes: StepMesh[]; timings: StepWorkerTimings }
  | { type: "error"; message: string };
