// Type declarations for occt-import-js and its WebAssembly asset.

declare module "occt-import-js" {
  export interface OcctMesh {
    name?: string;
    color?: [number, number, number];
    attributes: {
      position: { array: number[] | Float32Array<ArrayBuffer> };
      normal?: { array: number[] | Float32Array<ArrayBuffer> };
    };
    index?: { array: number[] | Uint16Array<ArrayBuffer> | Uint32Array<ArrayBuffer> };
  }

  export interface OcctResult {
    success: boolean;
    root?: unknown;
    meshes: OcctMesh[];
  }

  export interface OcctInstance {
    ReadStepFile(fileBuffer: Uint8Array, params?: unknown): OcctResult;
    ReadIgesFile(fileBuffer: Uint8Array, params?: unknown): OcctResult;
    ReadBrepFile(fileBuffer: Uint8Array, params?: unknown): OcctResult;
  }

  export interface OcctInitOptions {
    locateFile?: (path: string, prefix?: string) => string;
  }

  export default function init(options?: OcctInitOptions): Promise<OcctInstance>;
}

declare module "*occt-import-js.wasm?url" {
  const url: string;
  export default url;
}
