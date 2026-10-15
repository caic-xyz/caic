// Tests CAD parsing, Bambu filament assignments, printing-plate layouts, transforms, and model metrics.

import { describe, it } from "node:test";
import { expect } from "@tests/expect";

import { Box3, Color, LineSegments, Mesh, MeshPhongMaterial, Vector3 } from "three";
import type { GeometryGroup } from "three";
import { strToU8, strFromU8, unzipSync, zipSync } from "three/addons/libs/fflate.module.js";
import { create3mfFixture, createBambu3mfFixture, createPaintedBambu3mfFixture } from "../tests/cad-fixture";
import {
  computeModelInfo,
  createPlateGroup,
  createStlGroup,
  isWebGLSupported,
  parseCad,
  parseStl,
  prepareModelShading,
} from "./cad";

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

  it("parses 3MF component instances with build transforms and base colors", async () => {
    const { group, plates } = await parseCad(create3mfFixture().buffer, "assembly.3MF");
    expect(plates).toEqual([]);
    expect(computeModelInfo(group)).toEqual({
      triangleCount: 2,
      meshCount: 2,
      dimensions: { x: 30, y: 20, z: 0 },
    });
    group.traverse((child) => {
      if (child instanceof Mesh) {
        expect(child.material.color.getHex()).toBe(0xff0000);
      }
    });
  });

  it("uses Bambu part overrides, object inheritance, and instance-local colors without replacing standard materials", async () => {
    const { group } = await parseCad(createBambu3mfFixture().buffer, "bambu.3mf");
    const colors: number[] = [];
    const materials: unknown[] = [];
    group.traverse((child) => {
      if (child instanceof Mesh) {
        colors.push(child.material.color.getHex());
        materials.push(child.material);
      }
    });
    expect(colors).toEqual([0xffffff, 0x482960, 0xff0000, 0xe4bd68]);
    expect(materials[0]).not.toBe(materials[3]);
    expect(computeModelInfo(group)).toEqual({ triangleCount: 4, meshCount: 4, dimensions: { x: 70, y: 20, z: 0 } });
  });

  it("preserves a standard base material whose name matches the loader fallback", async () => {
    const files = unzipSync(createBambu3mfFixture());
    files["3D/Objects/parts.model"] = strToU8(
      strFromU8(files["3D/Objects/parts.model"]).replace('name="Red"', 'name="__DEFAULT"'),
    );
    const { group } = await parseCad(zipSync(files).buffer, "bambu.3mf");
    const colors: number[] = [];
    group.traverse((child) => {
      if (child instanceof Mesh) colors.push(child.material.color.getHex());
    });
    expect(colors).toEqual([0xffffff, 0x482960, 0xff0000, 0xe4bd68]);
  });

  it("colors only the fallback mesh of a mixed-material part", async () => {
    const files = unzipSync(createBambu3mfFixture());
    const xml = strFromU8(files["3D/Objects/parts.model"])
      .replace('id="5" type="model" pid="10" pindex="0"', 'id="5" type="model"')
      .replace(
        /(<object id="5"[\s\S]*?<triangles>)[\s\S]*?(<\/triangles>)/,
        '$1<triangle v1="0" v2="1" v3="2" pid="10" p1="0"/><triangle v1="0" v2="1" v3="2"/>$2',
      );
    files["3D/Objects/parts.model"] = strToU8(xml);
    const { group } = await parseCad(zipSync(files).buffer, "bambu.3mf");
    const colors: number[] = [];
    group.traverse((child) => {
      if (child instanceof Mesh) colors.push(child.material.color.getHex());
    });
    expect(colors).toEqual([0xffffff, 0x482960, 0xff0000, 0xe4bd68, 0xe4bd68]);
  });

  it("uses per-face Bambu paint instead of the inherited filament", async () => {
    const { group } = await parseCad(createPaintedBambu3mfFixture("8").buffer, "painted.3mf");
    const colors: number[] = [];
    group.traverse((child) => {
      if (child instanceof Mesh) {
        const materials = Array.isArray(child.material) ? child.material : [child.material];
        if (child.geometry.groups.length) {
          child.geometry.groups.forEach((range: GeometryGroup) =>
            colors.push(materials[range.materialIndex ?? 0].color.getHex()),
          );
        } else {
          colors.push(materials[0].color.getHex());
        }
      }
    });
    expect(colors).toEqual([0xe4bd68, 0x482960, 0xff0000, 0xe4bd68]);
    expect(computeModelInfo(group).triangleCount).toBe(4);
  });

  it("gives black display materials reflectance and highlights without changing source colors", async () => {
    const files = unzipSync(createBambu3mfFixture());
    files["Metadata/project_settings.config"] = strToU8(
      JSON.stringify({ filament_colour: Array.from({ length: 3 }, () => `#${new Color(0).getHexString()}`) }),
    );
    const { group } = await parseCad(zipSync(files).buffer, "bambu.3mf");
    const sources: MeshPhongMaterial[] = [];
    group.traverse((child) => {
      if (child instanceof Mesh && child.material instanceof MeshPhongMaterial) sources.push(child.material);
    });
    expect(sources[0].color.getHex()).toBe(0);
    group.add(group.children[0].clone()); // Shared instances must receive the same display material.
    prepareModelShading(group);
    const displayed: MeshPhongMaterial[] = [];
    group.traverse((child) => {
      if (child instanceof Mesh && child.material instanceof MeshPhongMaterial) displayed.push(child.material);
    });
    expect(displayed[0]).not.toBe(sources[0]);
    expect(displayed[0].color.r).toBeGreaterThan(0);
    expect(displayed[0].color.r).toBeLessThan(0.03);
    expect(displayed[0].specular.r).toBeGreaterThan(sources[0].specular.r);
    expect(displayed[0]).toBe(displayed[4]);
    expect(sources[0].color.getHex()).toBe(0);
    expect(displayed[2]).toBe(sources[2]); // Standard red material stays unchanged.
  });

  it("positions named plates in Bambu's grid without changing model metrics", async () => {
    const { group, plates } = await parseCad(createBambu3mfFixture().buffer, "bambu.3mf");
    expect(plates.map((plate) => [plate.id, plate.name])).toEqual([
      [1, "Panel"],
      [2, "Frame"],
      [3, "Spare"],
    ]);
    expect(plates[1].outline[0]).toEqual({ x: 60, y: 0 });
    expect(plates[2].outline[0]).toEqual({ x: 0, y: -48 });
    const beds = createPlateGroup(plates);
    expect(beds.children).toHaveLength(9); // Each bed has a surface, outline, and grid.
    const surface = beds.children[0];
    expect(surface).toBeInstanceOf(Mesh);
    if (!(surface instanceof Mesh)) throw new Error("Missing plate surface");
    expect(surface.material.transparent).toBe(true);
    expect(surface.material.opacity).toBeGreaterThan(0);
    expect(surface.material.opacity).toBeLessThan(1);
    expect(surface.material.depthWrite).toBe(false);
    const grid = beds.children[2];
    expect(grid).toBeInstanceOf(LineSegments);
    if (!(grid instanceof LineSegments)) throw new Error("Missing plate grid");
    const positions = grid.geometry.getAttribute("position");
    expect(positions.count).toBe(14); // Four vertical and three horizontal interior lines.
    expect([positions.getX(0), positions.getY(0), positions.getX(1), positions.getY(1)]).toEqual([10, 0, 10, 40]);
    expect([positions.getX(2), positions.getY(2), positions.getX(3), positions.getY(3)]).toEqual([20, 0, 20, 40]);
    const box = new Box3().setFromObject(beds);
    expect(box.getSize(new Vector3()).x).toBe(110);
    expect(box.getSize(new Vector3()).y).toBe(88);
    expect(computeModelInfo(group).triangleCount).toBe(4);
  });

  it("clips grid lines around concave plate cutouts", () => {
    const beds = createPlateGroup([
      {
        id: 1,
        name: "U",
        outline: [
          { x: 0, y: 0 },
          { x: 30, y: 0 },
          { x: 30, y: 30 },
          { x: 20, y: 30 },
          { x: 20, y: 10 },
          { x: 10, y: 10 },
          { x: 10, y: 30 },
          { x: 0, y: 30 },
        ],
      },
    ]);
    const grid = beds.children[2];
    if (!(grid instanceof LineSegments)) throw new Error("Missing plate grid");
    const positions = grid.geometry.getAttribute("position");
    const segments: number[][] = [];
    for (let i = 0; i < positions.count; i += 2) {
      if (positions.getY(i) === 20 && positions.getY(i + 1) === 20) {
        segments.push([positions.getX(i), positions.getX(i + 1)]);
      }
    }
    expect(segments).toEqual([
      [0, 10],
      [20, 30],
    ]);
  });

  it("rejects plate grids that exceed the bounded intersection work", () => {
    expect(() =>
      createPlateGroup([
        {
          id: 1,
          name: "Oversized",
          outline: [
            { x: 0, y: 0 },
            { x: 10_000_000, y: 0 },
            { x: 10_000_000, y: 10_000_000 },
            { x: 0, y: 10_000_000 },
          ],
        },
      ]),
    ).toThrow("Printing plate is too large");
  });

  it("shows no inferred plates without a printable area and rejects malformed outlines", async () => {
    const files = unzipSync(createBambu3mfFixture());
    const project = JSON.parse(strFromU8(files["Metadata/project_settings.config"])) as Record<string, unknown>;
    delete project.printable_area;
    files["Metadata/project_settings.config"] = strToU8(JSON.stringify(project));
    expect((await parseCad(zipSync(files).buffer, "bambu.3mf")).plates).toEqual([]);
    project.printable_area = ["0x0", "bad", "1x1"];
    files["Metadata/project_settings.config"] = strToU8(JSON.stringify(project));
    await expect(parseCad(zipSync(files).buffer, "bambu.3mf")).rejects.toThrow("Invalid printable area");
  });

  it("rejects invalid Bambu color metadata instead of showing a successful uncolored preview", async () => {
    const files = unzipSync(createBambu3mfFixture());
    files["Metadata/project_settings.config"] = strToU8(JSON.stringify({ filament_colour: [42] }));
    await expect(parseCad(zipSync(files).buffer, "bambu.3mf")).rejects.toThrow("Invalid filament palette");

    const outOfRange = unzipSync(createBambu3mfFixture());
    outOfRange["Metadata/model_settings.config"] = strToU8(
      strFromU8(outOfRange["Metadata/model_settings.config"]).replace('value="3"', 'value="9"'),
    );
    await expect(parseCad(zipSync(outOfRange).buffer, "bambu.3mf")).rejects.toThrow(
      "Extruder exceeds filament palette",
    );

    const malformed = unzipSync(createBambu3mfFixture());
    malformed["Metadata/model_settings.config"] = strToU8("<config>");
    await expect(parseCad(zipSync(malformed).buffer, "bambu.3mf")).rejects.toThrow("Invalid XML");
  });

  it("rejects malformed 3MF archives", async () => {
    await expect(parseCad(new TextEncoder().encode("not a zip").buffer, "broken.3mf")).rejects.toThrow();
  });
});
