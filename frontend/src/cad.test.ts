// Tests CAD 3D model parsing for STL and STEP formats and model metric calculations.

import { describe, it } from "node:test";
import { expect } from "@tests/expect";

import { computeModelInfo, createStlGroup, isWebGLSupported, parseStl, parseStep } from "./cad";

describe("cad", () => {
  it("detects WebGL support gracefully without throwing", () => {
    expect(typeof isWebGLSupported()).toBe("boolean");
  });

  it("parses ASCII STL buffers into geometries and computes model metrics", () => {
    const stlText = `solid test_triangle
facet normal 0 0 1
  outer loop
    vertex 0 0 0
    vertex 10 0 0
    vertex 0 20 0
  endloop
endfacet
endsolid test_triangle
`;
    const encoder = new TextEncoder();
    const buffer = encoder.encode(stlText).buffer;

    const geometry = parseStl(buffer);
    expect(geometry).not.toBeNull();
    expect(geometry.attributes.position.count).toBe(3);

    const group = createStlGroup(geometry);
    const info = computeModelInfo(group);
    expect(info.triangleCount).toBe(1);
    expect(info.meshCount).toBe(1);
    expect(info.dimensions.x).toBe(10);
    expect(info.dimensions.y).toBe(20);
    expect(info.dimensions.z).toBe(0);
  });

  it("parses STEP buffers into meshes and computes model metrics", async () => {
    const { readFileSync } = await import("node:fs");
    const { resolve } = await import("node:path");
    const filePath = resolve("node_modules/occt-import-js/test/testfiles/simple-basic-cube/cube.stp");
    const nodeBuf = readFileSync(filePath);
    const buffer = nodeBuf.buffer.slice(nodeBuf.byteOffset, nodeBuf.byteOffset + nodeBuf.byteLength);

    const group = await parseStep(buffer);
    expect(group).not.toBeNull();
    const info = computeModelInfo(group);
    expect(info.triangleCount).toBe(12);
    expect(info.meshCount).toBe(1);
    expect(info.dimensions.x).toBeGreaterThan(0);
    expect(info.dimensions.y).toBeGreaterThan(0);
    expect(info.dimensions.z).toBeGreaterThan(0);
  });
});
