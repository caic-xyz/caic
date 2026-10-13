// Streams Bambu face-paint provenance and renders indexed, material-grouped painted triangles.
import * as THREE from "three";
import { SaxesParser } from "saxes";

export interface BambuMeshInfo {
  triangleCount: number;
  hasDefaultTriangles: boolean;
  lattice: boolean;
  paint: Map<number, string>;
}

// A second geometry DOM is expensive for large models. Retain only sparse paint
// assignments and property provenance, and feed the XML parser bounded chunks.
export function readBambuMeshInfo(bytes: Uint8Array): Map<string, BambuMeshInfo> | null {
  if (bytes.byteLength > 256 * 1024 * 1024) throw new Error("3MF paint source exceeds 256 MiB");
  const chunkSize = 64 * 1024;
  const probe = new TextDecoder();
  let carry = "";
  let interesting = false;
  for (let i = 0; i < bytes.length; i += chunkSize) {
    const text = carry + probe.decode(bytes.subarray(i, i + chunkSize), { stream: true });
    if (/\b(pid|paint_color)\b/.test(text)) {
      interesting = true;
      break;
    }
    carry = text.slice(-12);
  }
  if (!interesting) return null;
  const objects = new Map<string, BambuMeshInfo>();
  let id: string | null = null;
  let current: BambuMeshInfo | null = null;
  let objectProperties = false;
  const parser = new SaxesParser({ xmlns: true });
  parser.on("opentag", (tag) => {
    if (tag.local === "object") {
      id = tag.attributes.id?.value ?? null;
      current = { triangleCount: 0, hasDefaultTriangles: false, lattice: false, paint: new Map() };
      objectProperties = tag.attributes.pid !== undefined;
    } else if (current && tag.local === "triangle") {
      const fallback = !objectProperties && tag.attributes.pid === undefined;
      current.hasDefaultTriangles ||= fallback;
      const paint = tag.attributes.paint_color?.value;
      if (fallback && paint !== undefined && paint !== "") current.paint.set(current.triangleCount, paint);
      current.triangleCount++;
    } else if (current && tag.local === "beamlattice") {
      current.lattice = true;
    }
  });
  parser.on("closetag", (tag) => {
    if (tag.local === "object" && current) {
      if (id === null || objects.has(id)) throw new Error("Invalid object ID in 3MF paint source");
      objects.set(id, current);
      current = null;
      id = null;
    }
  });
  const decoder = new TextDecoder();
  for (let i = 0; i < bytes.length; i += chunkSize) {
    parser.write(decoder.decode(bytes.subarray(i, i + chunkSize), { stream: true }));
  }
  parser.write(decoder.decode()).close();
  return objects;
}

type PaintNode = { kind: "leaf"; state: number } | { kind: "split"; side: number; children: PaintNode[] };

// Bambu serializes nibbles right-to-left, with child triangles in reverse order.
// Low bits describe a split; high bits describe its special side or leaf filament.
// Source: BambuStudio TriangleSelector::serialize/deserialize and perform_split,
// https://github.com/bambulab/BambuStudio/blob/47a13eeb1d9af65ef2371e361c856e21d4f6ca72/src/libslic3r/TriangleSelector.cpp
function decodePaint(text: string, paletteSize: number): PaintNode {
  if (text.length > 65_536 || !/^[\dA-F]+$/i.test(text)) throw new Error("Invalid Bambu paint selector");
  let offset = text.length - 1;
  function nibble(): number {
    if (offset < 0) throw new Error("Truncated Bambu paint selector");
    return Number.parseInt(text[offset--], 16);
  }
  function node(depth: number): PaintNode {
    if (depth > 64) throw new Error("Bambu paint subdivision exceeds 64 levels");
    const code = nibble();
    const split = code & 3;
    if (split === 0) {
      let state = code >> 2;
      if (state === 3) {
        let extra = nibble();
        while (extra === 15) {
          state += 15;
          extra = nibble();
        }
        state += extra;
      }
      // Reject invalid metadata rather than silently drop a requested filament.
      if (state > paletteSize) throw new Error("Bambu face paint exceeds filament palette");
      return { kind: "leaf", state };
    }
    const side = code >> 2;
    if (side > 2 || (split === 3 && side !== 0)) throw new Error("Invalid Bambu paint split side");
    const children: PaintNode[] = new Array<PaintNode>(split + 1);
    for (let i = split; i >= 0; i--) children[i] = node(depth + 1);
    return { kind: "split", side, children };
  }
  const root = node(0);
  if (offset !== -1) throw new Error("Trailing data in Bambu paint selector");
  return root;
}

// Paint subdivisions change draw geometry, not the source model's triangle metric.
export class BambuPaintGeometry extends THREE.BufferGeometry {
  constructor(readonly sourceTriangleCount: number) {
    super();
  }
}

interface PaintedGeometry {
  geometry: THREE.BufferGeometry;
  states: number[];
}

// Reorder indices into one draw range per filament. Keep position attributes shared
// for whole-face painting; append midpoint vertices only when paint splits a face.
export class BambuFacePainter {
  private readonly colors: THREE.Color[];
  private readonly selectors = new Map<string, PaintNode>();
  private readonly geometries = new WeakMap<THREE.BufferGeometry, PaintedGeometry>();

  constructor(palette: string[]) {
    this.colors = palette.map((color) => new THREE.Color().setStyle(color, THREE.SRGBColorSpace));
  }

  paint(mesh: THREE.Mesh, info: BambuMeshInfo): void {
    if (info.paint.size === 0) return;
    if (!(mesh.material instanceof THREE.MeshPhongMaterial)) throw new Error("Invalid Bambu paint fallback material");
    let painted = this.geometries.get(mesh.geometry);
    if (!painted) {
      painted = this.geometry(mesh.geometry, info);
      this.geometries.set(mesh.geometry, painted);
    }
    const base = mesh.material;
    mesh.material = painted.states.map((state) => {
      if (state === 0) return base;
      const material = base.clone();
      material.color.copy(this.colors[state - 1]);
      return material;
    });
    mesh.geometry = painted.geometry;
  }

  private selector(text: string): PaintNode {
    let node = this.selectors.get(text);
    if (!node) {
      node = decodePaint(text, this.colors.length);
      this.selectors.set(text, node);
    }
    return node;
  }

  private geometry(source: THREE.BufferGeometry, info: BambuMeshInfo): PaintedGeometry {
    const index = source.index;
    const position = source.getAttribute("position");
    if (!index || index.count !== info.triangleCount * 3) throw new Error("3MF face paint geometry mapping mismatch");
    let extraTriangles = 0;
    function leaves(node: PaintNode): number {
      return node.kind === "leaf" ? 1 : node.children.reduce((total, child) => total + leaves(child), 0);
    }
    for (const text of info.paint.values()) extraTriangles += leaves(this.selector(text)) - 1;
    // Limit expansion independently of the original model's triangle count.
    if (extraTriangles > 1_000_000) throw new Error("Bambu painted geometry exceeds subdivision budget");
    const count = info.triangleCount + extraTriangles;
    const triangles = new Uint32Array(count * 3);
    const assignments = new Uint32Array(count);
    const counts = new Map<number, number>();
    let output = 0;
    let vertices: number[] | null = null;
    function emit(a: number, b: number, c: number, state: number) {
      triangles.set([a, b, c], output * 3);
      assignments[output++] = state;
      counts.set(state, (counts.get(state) ?? 0) + 1);
    }
    function midpoint(a: number, b: number): number {
      if (!vertices) {
        vertices = [];
        for (let i = 0; i < position.count; i++) vertices.push(position.getX(i), position.getY(i), position.getZ(i));
      }
      const id = vertices.length / 3;
      for (let axis = 0; axis < 3; axis++) vertices.push((vertices[a * 3 + axis] + vertices[b * 3 + axis]) / 2);
      return id;
    }
    function visit(a: number, b: number, c: number, node: PaintNode) {
      if (node.kind === "leaf") {
        emit(a, b, c, node.state);
        return;
      }
      const corners = [a, b, c];
      const x = corners[node.side];
      const y = corners[(node.side + 1) % 3];
      const z = corners[(node.side + 2) % 3];
      if (node.children.length === 2) {
        const yz = midpoint(y, z);
        visit(x, y, yz, node.children[0]);
        visit(yz, z, x, node.children[1]);
      } else if (node.children.length === 3) {
        const xy = midpoint(x, y);
        const zx = midpoint(z, x);
        visit(x, xy, zx, node.children[0]);
        visit(xy, y, zx, node.children[1]);
        visit(y, z, zx, node.children[2]);
      } else {
        const xy = midpoint(x, y);
        const yz = midpoint(y, z);
        const zx = midpoint(z, x);
        visit(x, xy, zx, node.children[0]);
        visit(xy, y, yz, node.children[1]);
        visit(yz, z, zx, node.children[2]);
        visit(xy, yz, zx, node.children[3]);
      }
    }
    for (let face = 0; face < info.triangleCount; face++) {
      const a = index.getX(face * 3);
      const b = index.getX(face * 3 + 1);
      const c = index.getX(face * 3 + 2);
      const text = info.paint.get(face);
      if (text === undefined) emit(a, b, c, 0);
      else visit(a, b, c, this.selector(text));
    }
    const geometry = new BambuPaintGeometry(info.triangleCount);
    if (vertices) {
      geometry.setAttribute("position", new THREE.Float32BufferAttribute(vertices, 3));
    } else {
      for (const [name, attribute] of Object.entries(source.attributes)) geometry.setAttribute(name, attribute);
    }
    const states = Array.from(counts.keys()).sort((a, b) => a - b);
    const offsets = new Map<number, number>();
    let offset = 0;
    states.forEach((state, material) => {
      const size = (counts.get(state) ?? 0) * 3;
      geometry.addGroup(offset, size, material);
      offsets.set(state, offset);
      offset += size;
    });
    const ordered = new Uint32Array(triangles.length);
    for (let face = 0; face < count; face++) {
      const state = assignments[face];
      const start = offsets.get(state);
      if (start === undefined) throw new Error("Missing Bambu paint material range");
      ordered.set(triangles.subarray(face * 3, face * 3 + 3), start);
      offsets.set(state, start + 3);
    }
    geometry.setIndex(new THREE.BufferAttribute(ordered, 1));
    return { geometry, states };
  }
}
