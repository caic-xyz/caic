// Defines STEP worker requests, transferable mesh buffers, and observable loading stages.

export interface CadLoadProgress {
  stage: "download" | "decompress" | "parse" | "meshes" | "render";
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

export interface StepRequest {
  taskId: string;
  path: string;
}

export type StepMessage =
  | { type: "progress"; progress: CadLoadProgress }
  | { type: "result"; meshes: StepMesh[] }
  | { type: "error"; message: string };
