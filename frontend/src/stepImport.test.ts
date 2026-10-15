// Tests STEP worker geometry packing, compressed inputs, zero-copy assembly, and cancelled mesh setup.
import { describe, it } from "node:test";
import { readFileSync } from "node:fs";
import { BufferGeometry, Mesh } from "three";
import { gzipSync, zipSync } from "three/addons/libs/fflate.module.js";
import { expect, vi } from "@tests/expect";
import { importStep } from "./stepImport";
import { decompressStep } from "./stepCompression";
import { computeModelInfo, createStepGroup, disposeCadGroup } from "./cad";

const cube = readFileSync("node_modules/occt-import-js/test/testfiles/simple-basic-cube/cube.stp");

describe("STEP import", () => {
  it("packs geometry, normals, and bounds and shares its buffers with Three.js", async () => {
    const data = await importStep(cube);
    const group = await createStepGroup(data, new AbortController().signal, () => {});
    const info = computeModelInfo(group);
    expect(info.triangleCount).toBe(12);
    expect(info.meshCount).toBe(1);
    expect(info.dimensions.x).toBeGreaterThan(0);
    expect(info.dimensions.y).toBeGreaterThan(0);
    expect(info.dimensions.z).toBeGreaterThan(0);
    const mesh = group.children[0];
    expect(mesh).toBeInstanceOf(Mesh);
    if (!(mesh instanceof Mesh)) throw new Error("Missing mesh");
    expect(mesh.geometry.attributes.position.array).toBe(data[0].positions);
    expect(mesh.geometry.attributes.normal.array).toBe(data[0].normals);
    expect(mesh.geometry.index?.array).toBe(data[0].indices);
    expect(mesh.geometry.boundingBox).not.toBeNull();
    expect(mesh.geometry.boundingSphere).not.toBeNull();
    disposeCadGroup(group);
  });

  it("imports compressed STEP variants through the same geometry packer", async () => {
    for (const path of ["cube.stpz", "cube.step.zip", "cube.stp.zip", "cube.step.gz", "cube.stp.gz"]) {
      const compressed = path.endsWith(".gz") ? gzipSync(cube) : zipSync({ "models/cube.STP": cube });
      const data = await importStep(await decompressStep(compressed.buffer as ArrayBuffer, path, null));
      expect(data).toHaveLength(1);
      expect(data[0].indices?.length).toBe(36);
    }
  });

  it("handles concatenated gzip members before importing the complete model", async () => {
    const middle = Math.floor(cube.length / 2);
    const members = [gzipSync(cube.subarray(0, middle)), gzipSync(cube.subarray(middle))];
    const joined = new Uint8Array(members[0].length + members[1].length);
    joined.set(members[0]);
    joined.set(members[1], members[0].length);
    expect(await importStep(await decompressStep(joined, "cube.step.gz", null))).toHaveLength(1);
  });

  it("yields while assembling many meshes and disposes a cancelled partial model", async () => {
    const data = await importStep(cube);
    const controller = new AbortController();
    const disposed = vi.spyOn(BufferGeometry.prototype, "dispose");
    const timer = setTimeout(() => controller.abort(), 0);
    try {
      await expect(
        createStepGroup(
          Array.from({ length: 2000 }, () => data[0]),
          controller.signal,
          () => {},
        ),
      ).rejects.toThrow();
      expect(disposed).toHaveBeenCalled();
    } finally {
      clearTimeout(timer);
      disposed.mockRestore();
    }
  });

  it("rejects malformed STEP and aborts mesh assembly without retaining a partial model", async () => {
    await expect(importStep(new TextEncoder().encode("not STEP"))).rejects.toThrow("Failed to parse STEP");
    const data = await importStep(cube);
    const controller = new AbortController();
    controller.abort();
    await expect(createStepGroup(data, controller.signal, () => {})).rejects.toThrow();
  });
});
