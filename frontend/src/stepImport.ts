// Imports triangular faceted STEP directly, falls back to OCCT for other models, and packs transferable geometry.

import occtimportjs from "occt-import-js";
import type { OcctInstance, OcctResult } from "occt-import-js";
import occtWasmUrl from "occt-import-js/dist/occt-import-js.wasm?url";
import type { StepMesh } from "./stepProtocol";
import { readFacetedStep } from "./stepFacets";
import { createStepMesh } from "./stepMesh";

let occtPromise: Promise<OcctInstance> | null = null;

export async function importStep(buffer: ArrayBuffer | Uint8Array<ArrayBuffer>): Promise<StepMesh[]> {
  const bytes = buffer instanceof Uint8Array ? buffer : new Uint8Array(buffer);
  const facets = readFacetedStep(bytes);
  if (facets) return facets;
  // End the input-owning frame before packing results so a large decoded file
  // does not remain referenced while the numeric geometry arrays are converted.
  return readStep(bytes).then(packStepMeshes);
}

async function readStep(buffer: ArrayBuffer | Uint8Array<ArrayBuffer>): Promise<OcctResult> {
  if (!occtPromise) {
    const isNode = typeof process !== "undefined" && Boolean(process.versions?.node);
    occtPromise = occtimportjs({
      locateFile: (name, prefix) => (!isNode && name.endsWith(".wasm") ? occtWasmUrl : (prefix ?? "") + name),
    });
  }
  const occt = await occtPromise;
  return occt.ReadStepFile(buffer instanceof Uint8Array ? buffer : new Uint8Array(buffer), null);
}

function packStepMeshes(result: OcctResult): StepMesh[] {
  if (!result?.success || !result.meshes.length)
    throw new Error("Failed to parse STEP file: invalid geometry or syntax");
  const meshes: StepMesh[] = new Array(result.meshes.length);
  // Remove each source mesh as it is packed so the original numeric arrays
  // can be collected instead of retaining two complete geometry representations.
  while (result.meshes.length) {
    const index = result.meshes.length - 1;
    const source = result.meshes[index];
    result.meshes.pop();
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
