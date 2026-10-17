// Packs typed STEP geometry with flat or supplied normals and validated bounds for worker transfer.
import { BufferAttribute, BufferGeometry } from "three";
import type { StepImportTimings, StepMesh } from "./stepProtocol";

export function createStepMesh(
  positions: Float32Array<ArrayBuffer>,
  normals: Float32Array<ArrayBuffer> | null,
  indices: Uint16Array<ArrayBuffer> | Uint32Array<ArrayBuffer> | null,
  color: [number, number, number] | null,
  timings?: StepImportTimings,
): StepMesh {
  const started = timings ? performance.now() : 0;
  const geometry = new BufferGeometry();
  geometry.setAttribute("position", new BufferAttribute(positions, 3));
  if (indices) geometry.setIndex(new BufferAttribute(indices, 1));
  if (normals) geometry.setAttribute("normal", new BufferAttribute(normals, 3));
  else geometry.computeVertexNormals();
  geometry.computeBoundingBox();
  geometry.computeBoundingSphere();
  const box = geometry.boundingBox;
  const sphere = geometry.boundingSphere;
  if (!box || !sphere || box.isEmpty() || !Number.isFinite(sphere.radius)) throw new Error("Invalid STEP mesh bounds");
  if (timings) timings.packingMs += performance.now() - started;
  return {
    positions,
    normals: geometry.getAttribute("normal").array as Float32Array<ArrayBuffer>,
    indices,
    color,
    min: [box.min.x, box.min.y, box.min.z],
    max: [box.max.x, box.max.y, box.max.z],
    sphere: { center: [sphere.center.x, sphere.center.y, sphere.center.z], radius: sphere.radius },
  };
}
