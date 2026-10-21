// Tests Bambu painted-face inheritance, subdivision geometry, extended selectors, and malformed streams.
import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { Mesh, MeshPhongMaterial, Vector3 } from "three";
import type { Group } from "three";
import { strFromU8, strToU8, unzipSync, zipSync } from "three/addons/libs/fflate.module.js";
import { expect } from "@tests/expect";
import { createPaintedBambu3mfFixture as archive } from "../tests/cad-fixture";
import { computeModelInfo, parseCad } from "./cad";
import { readBambuMeshInfo } from "./bambuPaint";

function meshes(group: Group): Mesh[] {
  const out: Mesh[] = [];
  group.traverse((child) => {
    if (child instanceof Mesh) out.push(child);
  });
  return out;
}

function areaByColor(mesh: Mesh): Map<number, number> {
  const geometry = mesh.geometry;
  const positions = geometry.getAttribute("position");
  const index = geometry.index;
  if (!index) throw new Error("Missing painted triangle indices");
  const materials = Array.isArray(mesh.material) ? mesh.material : [mesh.material];
  const areas = new Map<number, number>();
  const a = new Vector3();
  const b = new Vector3();
  const c = new Vector3();
  for (const range of geometry.groups) {
    const material = materials[range.materialIndex ?? 0];
    if (!(material instanceof MeshPhongMaterial)) throw new Error("Missing painted Phong material");
    const color = material.color.getHex();
    for (let i = range.start; i < range.start + range.count; i += 3) {
      a.fromBufferAttribute(positions, index.getX(i));
      b.fromBufferAttribute(positions, index.getX(i + 1));
      c.fromBufferAttribute(positions, index.getX(i + 2));
      const normal = b.sub(a).cross(c.sub(a));
      expect(normal.z).toBeGreaterThan(0);
      areas.set(color, (areas.get(color) ?? 0) + normal.length() / 2);
    }
  }
  return areas;
}

describe("Bambu face paint", () => {
  it("keeps unpainted faces on each instance's inherited filament while sharing indexed geometry", async () => {
    const files = unzipSync(archive("8"));
    assert.ok(files["3D/Objects/parts.model"] !== undefined);
    files["3D/Objects/parts.model"] = strToU8(
      strFromU8(files["3D/Objects/parts.model"]).replace(
        'paint_color="8"/>',
        'paint_color="8"/><triangle v1="0" v2="1" v3="2"/>',
      ),
    );
    const { group } = await parseCad(zipSync(files).buffer, "mixed-paint.3mf");
    const all = meshes(group);
    assert.ok(all[0] !== undefined);
    expect(areaByColor(all[0])).toEqual(
      new Map([
        [0xffffff, 100],
        [0xe4bd68, 100],
      ]),
    );
    assert.ok(all[3] !== undefined);
    expect(areaByColor(all[3])).toEqual(new Map([[0xe4bd68, 200]]));
    expect(all[0].geometry).toBe(all[3].geometry);
    expect(all[0].geometry.getAttribute("position").count).toBe(3);
    expect(computeModelInfo(group).triangleCount).toBe(6);
  });

  for (const [selector, midpoint] of [
    ["481", [5, 10]],
    ["485", [0, 10]],
    ["489", [5, 0]],
  ] as const) {
    it(`decodes one-side subdivision ${selector} with correct midpoint, colors, and winding`, async () => {
      const { group } = await parseCad(archive(selector).buffer, "split.3mf");
      const mesh = meshes(group)[0];
      assert.ok(mesh !== undefined);
      expect(areaByColor(mesh)).toEqual(
        new Map([
          [0xffffff, 50],
          [0xe4bd68, 50],
        ]),
      );
      const position = mesh.geometry.getAttribute("position");
      expect([position.getX(3), position.getY(3)]).toEqual([...midpoint]);
      expect(computeModelInfo(group).triangleCount).toBe(4);
      expect(computeModelInfo(group).dimensions).toEqual({ x: 70, y: 20, z: 0 });
    });
  }

  it("decodes two-side subdivision in reverse serialized child order", async () => {
    const { group } = await parseCad(archive("4842").buffer, "split.3mf");
    const meshesItem = meshes(group)[0];
    assert.ok(meshesItem !== undefined);
    expect(areaByColor(meshesItem)).toEqual(
      new Map([
        [0xffffff, 75],
        [0xe4bd68, 25],
      ]),
    );
  });

  it("decodes four-child subdivision without changing surface area or source triangle metrics", async () => {
    const { group } = await parseCad(archive("48483").buffer, "split.3mf");
    const meshesItem = meshes(group)[0];
    assert.ok(meshesItem !== undefined);
    expect(areaByColor(meshesItem)).toEqual(
      new Map([
        [0xffffff, 50],
        [0xe4bd68, 50],
      ]),
    );
    expect(computeModelInfo(group).triangleCount).toBe(4);
  });

  it("decodes nested splits without reversing the painted region", async () => {
    const { group } = await parseCad(archive("44811").buffer, "nested.3mf");
    const meshesItem = meshes(group)[0];
    assert.ok(meshesItem !== undefined);
    expect(areaByColor(meshesItem)).toEqual(
      new Map([
        [0xffffff, 75],
        [0xe4bd68, 25],
      ]),
    );
  });

  it("uses the object filament for state zero and decodes an extended filament state", async () => {
    const unpainted = await parseCad(archive("0").buffer, "zero.3mf");
    const all = meshes(unpainted.group);
    assert.ok(all[0] !== undefined);
    expect(areaByColor(all[0])).toEqual(new Map([[0xffffff, 100]]));
    assert.ok(all[3] !== undefined);
    expect(areaByColor(all[3])).toEqual(new Map([[0xe4bd68, 100]]));
    const purple = await parseCad(archive("0C").buffer, "extended.3mf");
    const meshesItem1 = meshes(purple.group)[0];
    assert.ok(meshesItem1 !== undefined);
    expect(areaByColor(meshesItem1)).toEqual(new Map([[0x482960, 100]]));

    const files = unzipSync(archive("0FC"));
    assert.ok(files["Metadata/project_settings.config"] !== undefined);
    const project = JSON.parse(strFromU8(files["Metadata/project_settings.config"])) as { filament_colour: string[] };
    const colors = project.filament_colour;
    const base = colors[0],
      extendedColor = colors[2];
    assert.ok(base !== undefined && extendedColor !== undefined);
    project.filament_colour = Array.from({ length: 18 }, (_, i) => (i === 17 ? extendedColor : base));
    files["Metadata/project_settings.config"] = strToU8(JSON.stringify(project));
    const extended = await parseCad(zipSync(files).buffer, "extended.3mf");
    const meshesItem2 = meshes(extended.group)[0];
    assert.ok(meshesItem2 !== undefined);
    expect(areaByColor(meshesItem2)).toEqual(new Map([[0x482960, 100]]));
  });

  for (const selector of ["00", "C", "D", "F", "X", "1", "9C"]) {
    it(`rejects invalid or incomplete paint selector ${selector}`, async () => {
      await expect(parseCad(archive(selector).buffer, "broken.3mf")).rejects.toThrow();
    });
  }

  it("bounds subdivision depth", async () => {
    const selector = "0".repeat(66) + "1".repeat(65);
    await expect(parseCad(archive(selector).buffer, "deep.3mf")).rejects.toThrow("64 levels");
  });

  it("reads paint attributes across chunk boundaries and XML entities without keeping a geometry DOM", () => {
    const xml = `<model><resources><object id="1"><mesh><triangles>${" ".repeat(65_480)}<triangle v1="0" v2="1" v3="2" paint_color="&#56;"/></triangles></mesh></object></resources></model>`;
    const info = readBambuMeshInfo(strToU8(xml))?.get("1");
    expect(info?.triangleCount).toBe(1);
    expect(info?.paint.get(0)).toBe("8");
    expect(() => readBambuMeshInfo(strToU8('<model><object id="1" pid="1">'))).toThrow();
  });
});
