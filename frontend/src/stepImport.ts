// Imports planar faceted STEP directly, falls back to OCCT for other models, and packs transferable geometry.

import occtimportjs from "occt-import-js";
import type { OcctInstance, OcctResult } from "occt-import-js";
import occtWasmUrl from "occt-import-js/dist/occt-import-js.wasm?url";
import type { CadProgress, StepImportTimings, StepMesh } from "./stepProtocol";
import type { FacetedStepReader } from "./stepFacets";
import { createStepMesh } from "./stepMesh";

let occtPromise: Promise<OcctInstance> | null = null;

export async function importStep(
  reader: FacetedStepReader,
  timings?: StepImportTimings,
  progress?: CadProgress,
): Promise<StepMesh[]> {
  const started = performance.now();
  const previousPacking = timings?.packingMs ?? 0;
  const facets = reader.meshes(timings, progress);
  if (timings) timings.directParseMs += performance.now() - started - (timings.packingMs - previousPacking);
  if (facets) return facets;
  progress?.({ stage: "parse", parseStep: "occt-init", completed: null, total: null });
  // End the input-owning frame before packing results so a large decoded file
  // does not remain referenced while the numeric geometry arrays are converted.
  return readStep(reader.takeContiguous(), timings, progress).then((result) => {
    const packingStarted = performance.now();
    const meshes = packStepMeshes(result);
    if (timings) timings.packingMs += performance.now() - packingStarted;
    return meshes;
  });
}

async function readStep(
  buffer: ArrayBuffer | Uint8Array<ArrayBuffer>,
  timings: StepImportTimings | undefined,
  progress: CadProgress | undefined,
): Promise<OcctResult> {
  const initStarted = performance.now();
  if (!occtPromise) {
    const isNode = typeof process !== "undefined" && Boolean(process.versions?.node);
    occtPromise = occtimportjs({
      locateFile: (name, prefix) => (!isNode && name.endsWith(".wasm") ? occtWasmUrl : (prefix ?? "") + name),
    });
  }
  const occt = await occtPromise;
  const parseStarted = performance.now();
  if (timings) timings.occtInitMs += parseStarted - initStarted;
  progress?.({ stage: "parse", parseStep: "occt", completed: null, total: null });
  const result = occt.ReadStepFile(buffer instanceof Uint8Array ? buffer : new Uint8Array(buffer), null);
  if (timings) timings.occtParseMs += performance.now() - parseStarted;
  return result;
}

function packStepMeshes(result: OcctResult): StepMesh[] {
  if (!result?.success || !result.meshes.length)
    throw new Error("Failed to parse STEP file: invalid geometry or syntax");
  const meshes: StepMesh[] = new Array(result.meshes.length);
  // Remove each source mesh as it is packed so the original numeric arrays
  // can be collected instead of retaining two complete geometry representations.
  while (result.meshes.length) {
    const index = result.meshes.length - 1;
    const source = result.meshes.pop();
    if (!source) throw new Error("Missing STEP mesh");
    const positions =
      source.attributes.position.array instanceof Float32Array
        ? source.attributes.position.array
        : new Float32Array(source.attributes.position.array);
    const sourceIndices = source.index?.array;
    const indices = sourceIndices
      ? sourceIndices instanceof Uint16Array || sourceIndices instanceof Uint32Array
        ? sourceIndices
        : new Uint32Array(sourceIndices)
      : null;
    const sourceNormals = source.attributes.normal?.array;
    const normals = sourceNormals
      ? sourceNormals instanceof Float32Array
        ? sourceNormals
        : new Float32Array(sourceNormals)
      : null;
    meshes[index] = createStepMesh(positions, normals, indices, source.color ?? null);
  }
  return meshes;
}
