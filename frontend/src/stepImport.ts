// Parses STEP with the unchanged OCCT importer and prepares transferable geometry entirely off the UI thread.

import { BufferAttribute, BufferGeometry } from "three";
import occtimportjs from "occt-import-js";
import type { OcctInstance, OcctResult } from "occt-import-js";
import occtWasmUrl from "occt-import-js/dist/occt-import-js.wasm?url";
import type { StepMesh } from "./stepProtocol";

let occtPromise: Promise<OcctInstance> | null = null;

export function importStep(buffer: ArrayBuffer | Uint8Array<ArrayBuffer>): Promise<StepMesh[]> {
  // End the input-owning frame before packing results so a large decoded file
  // does not remain referenced while the numeric geometry arrays are converted.
  return readStep(buffer).then(packStepMeshes);
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
    const geometry = new BufferGeometry();
    geometry.setAttribute("position", new BufferAttribute(positions, 3));
    if (indices) geometry.setIndex(new BufferAttribute(indices, 1));
    const sourceNormals = source.attributes.normal?.array;
    if (sourceNormals) {
      geometry.setAttribute(
        "normal",
        new BufferAttribute(sourceNormals instanceof Float32Array ? sourceNormals : new Float32Array(sourceNormals), 3),
      );
    } else {
      geometry.computeVertexNormals();
    }
    geometry.computeBoundingBox();
    geometry.computeBoundingSphere();
    const box = geometry.boundingBox;
    const sphere = geometry.boundingSphere;
    if (!box || !sphere || box.isEmpty() || !Number.isFinite(sphere.radius))
      throw new Error("Invalid STEP mesh bounds");
    meshes[index] = {
      positions,
      normals: geometry.getAttribute("normal").array as Float32Array<ArrayBuffer>,
      indices,
      color: source.color ?? null,
      min: [box.min.x, box.min.y, box.min.z],
      max: [box.max.x, box.max.y, box.max.z],
      sphere: { center: [sphere.center.x, sphere.center.y, sphere.center.z], radius: sphere.radius },
    };
  }
  return meshes;
}
