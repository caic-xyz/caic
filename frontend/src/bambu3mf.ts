// Parses Bambu filament assignments, painted faces, and printing plates while preserving standard materials.
import * as THREE from "three";
import { strFromU8, unzipSync } from "three/addons/libs/fflate.module.js";
import { BambuFacePainter, readBambuMeshInfo } from "./bambuPaint";
import type { BambuMeshInfo } from "./bambuPaint";

const productionNamespace = "http://schemas.microsoft.com/3dmanufacturing/production/2015/06";

function parseXml(bytes: Uint8Array): Document {
  const doc = new DOMParser().parseFromString(strFromU8(bytes), "application/xml");
  if (doc.querySelector("parsererror")) throw new Error("Invalid XML in 3MF color metadata");
  return doc;
}

function extruder(node: Element): number | null {
  const value = node.querySelector(':scope > metadata[key="extruder"]')?.getAttribute("value");
  if (value === undefined) return null;
  const index = Number(value);
  if (!Number.isInteger(index) || index < 1) throw new Error("Invalid extruder in 3MF color metadata");
  return index;
}

export interface CadPlate {
  id: number;
  name: string;
  outline: { x: number; y: number }[];
}

function printingPlates(config: Document, area: unknown): CadPlate[] {
  const nodes = Array.from(config.querySelectorAll("config > plate"));
  if (nodes.length === 0 || area === undefined) return [];
  if (!Array.isArray(area) || area.length < 3) throw new Error("Invalid printable area in 3MF metadata");
  const outline = area.map((point: unknown) => {
    if (typeof point !== "string" || !/^-?\d+(?:\.\d+)?x-?\d+(?:\.\d+)?$/.test(point)) {
      throw new Error("Invalid printable area in 3MF metadata");
    }
    const [x, y] = point.split("x").map(Number);
    if (!Number.isFinite(x) || !Number.isFinite(y)) throw new Error("Invalid printable area in 3MF metadata");
    return { x, y };
  });
  const width = Math.max(...outline.map((p) => p.x)) - Math.min(...outline.map((p) => p.x));
  const depth = Math.max(...outline.map((p) => p.y)) - Math.min(...outline.map((p) => p.y));
  if (width <= 0 || depth <= 0) throw new Error("Empty printable area in 3MF metadata");
  // BambuStudio PartPlateList::compute_shape_position uses ceil(sqrt(count))
  // columns with a 20% gap, advancing rows along negative Y. Source:
  // https://github.com/bambulab/BambuStudio/blob/47a13eeb1d9af65ef2371e361c856e21d4f6ca72/src/slic3r/GUI/PartPlate.cpp
  const columns = Math.ceil(Math.sqrt(nodes.length));
  return nodes.map((plate, i) => {
    const id = Number(plate.querySelector('metadata[key="plater_id"]')?.getAttribute("value"));
    if (!Number.isInteger(id) || id < 1) throw new Error("Invalid printing plate ID in 3MF metadata");
    const name = plate.querySelector('metadata[key="plater_name"]')?.getAttribute("value") ?? "";
    return {
      id,
      name,
      outline: outline.map((p) => ({
        x: p.x + (i % columns) * width * 1.2,
        y: p.y - Math.floor(i / columns) * depth * 1.2,
      })),
    };
  });
}

export function applyBambuMetadata(group: THREE.Group, buffer: ArrayBuffer): CadPlate[] {
  const files = unzipSync(new Uint8Array(buffer), {
    filter: (file) =>
      file.name === "Metadata/project_settings.config" ||
      file.name === "Metadata/model_settings.config" ||
      file.name === "_rels/.rels",
  });
  const project = files["Metadata/project_settings.config"];
  const settings = files["Metadata/model_settings.config"];
  if (!project || !settings) return []; // Ordinary 3MF files have no slicer metadata.

  const data: unknown = JSON.parse(strFromU8(project));
  if (typeof data !== "object" || data === null) throw new Error("Invalid 3MF project settings");
  const config = parseXml(settings);
  const plates = printingPlates(config, "printable_area" in data ? data.printable_area : undefined);
  if (!("filament_colour" in data)) return plates;
  const palette = data.filament_colour;
  if (
    !Array.isArray(palette) ||
    !palette.every((color: unknown): color is string => typeof color === "string" && /^#[\da-f]{6}$/i.test(color))
  ) {
    throw new Error("Invalid filament palette in 3MF color metadata");
  }
  const colors: string[] = palette;
  const facePainter = new BambuFacePainter(colors);
  const rels = parseXml(files["_rels/.rels"]);
  const target = Array.from(rels.querySelectorAll("Relationship"))
    .find((rel) => rel.getAttribute("Type")?.endsWith("/3dmodel"))
    ?.getAttribute("Target");
  if (!target) throw new Error("Missing 3MF model relationship");
  const rootPath = target.replace(/^\//, "");
  const documents = new Map<string, Document>();
  const meshSources = new Map<string, Map<string, BambuMeshInfo> | null>();
  function meshInfo(path: string, id: string): BambuMeshInfo | null {
    if (!meshSources.has(path)) {
      const bytes = unzipSync(new Uint8Array(buffer), { filter: (file) => file.name === path })[path];
      if (!bytes) throw new Error("Missing model in 3MF color metadata");
      meshSources.set(path, readBambuMeshInfo(bytes));
    }
    const objects = meshSources.get(path);
    if (!objects) return null;
    const object = objects.get(id);
    if (!object) throw new Error("Missing object in 3MF color metadata");
    return object;
  }
  function document(path: string): Document {
    let doc = documents.get(path);
    if (!doc) {
      // Read only composite documents. Leaf geometry is already owned by the loader.
      const bytes = unzipSync(new Uint8Array(buffer), { filter: (file) => file.name === path })[path];
      if (!bytes) throw new Error("Missing model in 3MF color metadata");
      doc = parseXml(bytes);
      documents.set(path, doc);
    }
    return doc;
  }

  function paint(node: THREE.Object3D, index: number | null) {
    if (index === null) return;
    const color = colors[index - 1];
    if (color === undefined) throw new Error("Extruder exceeds filament palette in 3MF color metadata");
    node.traverse((child) => {
      if (!(child instanceof THREE.Mesh)) return;
      function material(source: THREE.Material): THREE.Material {
        if (!(source instanceof THREE.MeshPhongMaterial)) return source;
        const clone = source.clone(); // Instances share loader materials, but can use different filaments.
        clone.color.setStyle(color, THREE.SRGBColorSpace);
        return clone;
      }
      child.material = Array.isArray(child.material) ? child.material.map(material) : material(child.material);
    });
  }

  function walk(
    node: THREE.Object3D,
    path: string,
    id: string,
    index: number | null,
    parts: Map<string, number | null>,
  ) {
    const effective = parts.get(id) ?? index;
    if (node.children.every((child) => child instanceof THREE.Mesh)) {
      const info = meshInfo(path, id);
      // The pinned loader emits numeric property resources first, then its one
      // default mesh, then the optional lattice mesh. Names are user-controlled.
      const count = info ? Number(info.hasDefaultTriangles) + Number(info.lattice) : node.children.length;
      const fallback = count > 0 ? node.children.slice(-count) : [];
      fallback.forEach((mesh) => paint(mesh, effective));
      if (info?.hasDefaultTriangles && fallback[0] instanceof THREE.Mesh) facePainter.paint(fallback[0], info);
      return;
    }
    const object = Array.from(document(path).querySelectorAll("resources > object")).find(
      (o) => o.getAttribute("id") === id,
    );
    if (!object) throw new Error("Missing object in 3MF color metadata");
    const components = object.querySelectorAll("components > component");
    // The pinned ThreeMFLoader preserves build/component order in Group.children.
    if (components.length !== node.children.length) throw new Error("3MF component color mapping mismatch");
    components.forEach((component, i) => {
      const componentPath = component.getAttributeNS(productionNamespace, "path");
      walk(
        node.children[i],
        componentPath ? componentPath.replace(/^\//, "") : path,
        component.getAttribute("objectid") ?? "",
        effective,
        parts,
      );
    });
  }

  const items = document(rootPath).querySelectorAll("build > item");
  if (items.length !== group.children.length) throw new Error("3MF build color mapping mismatch");
  items.forEach((item, i) => {
    const id = item.getAttribute("objectid") ?? "";
    const object = Array.from(config.querySelectorAll("config > object")).find((o) => o.getAttribute("id") === id);
    if (!object) return;
    const parts = new Map<string, number | null>();
    object.querySelectorAll("part").forEach((part) => parts.set(part.getAttribute("id") ?? "", extruder(part)));
    walk(group.children[i], rootPath, id, extruder(object), parts);
  });
  return plates;
}
