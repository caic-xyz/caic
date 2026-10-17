// Tests complete faceted STEP imports, transformed instances, colors, units, winding, and conservative fallback.
import { describe, it } from "node:test";
import { readFileSync } from "node:fs";
import occtimportjs from "occt-import-js";
import { expect } from "@tests/expect";
import { createFacetedStep } from "../tests/step-fixture";
import { readFacetedStep } from "./stepFacets";
import { importStep } from "./stepImport";

const fixture = createFacetedStep(2, false);
const text = new TextDecoder().decode(fixture);
function parse(value: string) {
  return readFacetedStep(new TextEncoder().encode(value));
}
function imported(value: string) {
  const meshes = parse(value);
  if (!meshes) throw new Error("Expected a supported faceted STEP assembly");
  return meshes;
}
function match(pattern: RegExp): RegExpMatchArray {
  const result = text.match(pattern);
  if (!result) throw new Error("Missing STEP fixture entity");
  return result;
}

function normalDirections(normals: ArrayLike<number>): string[] {
  const directions = new Set<string>();
  for (let i = 0; i < normals.length; i += 3)
    directions.add(JSON.stringify([0, 1, 2].map((j) => Math.round(normals[i + j] * 1e5) / 1e5 + 0)));
  return [...directions].sort();
}

async function matchesOcct(value: string) {
  const meshes = imported(value);
  const occt = await occtimportjs();
  const expected = occt.ReadStepFile(new TextEncoder().encode(value), null);
  expect(expected.success).toBe(true);
  expect(expected.meshes).toHaveLength(meshes.length);
  for (let i = 0; i < meshes.length; i++) {
    const positions = expected.meshes[i].attributes.position.array;
    const min = [Infinity, Infinity, Infinity];
    const max = [-Infinity, -Infinity, -Infinity];
    for (let j = 0; j < positions.length; j++) {
      min[j % 3] = Math.min(min[j % 3], positions[j]);
      max[j % 3] = Math.max(max[j % 3], positions[j]);
    }
    expect(meshes[i].min).toEqual(min);
    expect(meshes[i].max).toEqual(max);
    const normals = expected.meshes[i].attributes.normal?.array;
    if (!normals) throw new Error("Missing OCCT normals");
    expect(normalDirections(meshes[i].normals)).toEqual(normalDirections(normals));
    const actualColor = meshes[i].color;
    const expectedColor = expected.meshes[i].color;
    if (!actualColor || !expectedColor) throw new Error("Missing STEP part color");
    for (let j = 0; j < 3; j++) expect(actualColor[j]).toBeCloseTo(expectedColor[j]);
  }
}

describe("faceted STEP", () => {
  it("imports every triangular body with assembly rotation, translation, and colors", async () => {
    const meshes = imported(text);
    expect(meshes).toHaveLength(2);
    expect(meshes[0].positions.length / 9).toBe(4);
    expect(meshes[0].min).toEqual([0, 0, 0]);
    expect(meshes[0].max).toEqual([1, 1, 1]);
    expect(meshes[1].min).toEqual([4, 0, 0]);
    expect(meshes[1].max).toEqual([5, 1, 1]);
    expect(meshes[0].color?.[0]).toBeCloseTo(0.0331048, 6);
    expect(meshes[1].color?.[0]).toBeCloseTo(0.6038273, 6);
    await matchesOcct(text);
  });

  it("supports SI length scaling and forward, reordered, and sparse entity references", () => {
    const meters = imported(text.replace(".MILLI.,.METRE.", "$,.METRE."));
    expect(meters[1].min).toEqual([4000, 0, 0]);
    expect(meters[1].max).toEqual([5000, 1000, 1000]);
    const lines = text.split("\n");
    const start = lines.indexOf("DATA;") + 1;
    const end = lines.indexOf("ENDSEC;", start);
    lines.splice(start, end - start, ...lines.slice(start, end).reverse());
    const reordered = lines.join("\n").replace(/#(\d+)/g, (_, id: string) => `#${Number(id) * 100003}`);
    expect(
      imported(reordered)
        .map((m) => m.max)
        .sort((a, b) => a[0] - b[0]),
    ).toEqual([
      [1, 1, 1],
      [5, 1, 1],
    ]);
  });

  it("emits every occurrence of a reused part with its own placement", () => {
    const meshes = imported(new TextDecoder().decode(createFacetedStep(3, true)));
    expect(meshes).toHaveLength(3);
    expect(meshes.map((mesh) => mesh.min)).toEqual([
      [0, 0, 0],
      [4, 0, 0],
      [10, 0, 0],
    ]);
    expect(meshes.map((mesh) => mesh.positions.length / 9)).toEqual([4, 4, 4]);
  });

  it("composes nested assembly placements instead of rendering children in local coordinates", () => {
    const [, root, context] = match(/^#(\d+)=SHAPE_REPRESENTATION\('',.*,#(\d+)\);$/m);
    const [, base] = match(/ITEM_DEFINED_TRANSFORMATION\('','',#(\d+),/);
    const [, z, x] = match(new RegExp(`#${base}=AXIS2_PLACEMENT_3D\\('',#\\d+,#(\\d+),#(\\d+)\\)`));
    const [, product] = match(/NEXT_ASSEMBLY_USAGE_OCCURRENCE\('[^']*','[^']*','',#(\d+),/);
    const [, app] = match(/PRODUCT_CONTEXT\('',#(\d+),/);
    const wrapper = `
#100000000=PRODUCT_CONTEXT('',#${app},'mechanical');
#100000001=PRODUCT('wrapper','wrapper','',(#100000000));
#100000002=PRODUCT_DEFINITION_FORMATION('','',#100000001);
#100000003=PRODUCT_DEFINITION_CONTEXT('part definition',#${app},'design');
#100000004=PRODUCT_DEFINITION('design','',#100000002,#100000003);
#100000005=PRODUCT_DEFINITION_SHAPE('','',#100000004);
#100000006=CARTESIAN_POINT('',(10,20,30));
#100000007=AXIS2_PLACEMENT_3D('',#100000006,#${z},#${x});
#100000008=SHAPE_REPRESENTATION('',(#${base},#100000007),#${context});
#100000009=SHAPE_DEFINITION_REPRESENTATION(#100000005,#100000008);
#100000010=ITEM_DEFINED_TRANSFORMATION('','',#${base},#100000007);
#100000011=(REPRESENTATION_RELATIONSHIP('','',#${root},#100000008)REPRESENTATION_RELATIONSHIP_WITH_TRANSFORMATION(#100000010)SHAPE_REPRESENTATION_RELATIONSHIP());
#100000012=NEXT_ASSEMBLY_USAGE_OCCURRENCE('wrapper','','',#100000004,#${product},$);
#100000013=PRODUCT_DEFINITION_SHAPE('','',#100000012);
#100000014=CONTEXT_DEPENDENT_SHAPE_REPRESENTATION(#100000011,#100000013);
`;
    const meshes = imported(text.replace("ENDSEC;\nEND-ISO-10303-21;", `${wrapper}ENDSEC;\nEND-ISO-10303-21;`));
    expect(meshes.map((mesh) => mesh.min)).toEqual([
      [10, 20, 30],
      [14, 20, 30],
    ]);
    expect(meshes.map((mesh) => mesh.max)).toEqual([
      [11, 21, 31],
      [15, 21, 31],
    ]);
  });

  it("handles multiline records, quoted semicolons, comments, and reversed surface sense", async () => {
    const spaced = text.replaceAll("=", "=\n").replace("DATA;", "DATA;\n/* a ; and ' inside a comment */");
    expect(parse(spaced)).toHaveLength(2);
    const reversedText = text
      .replace(/(FACE_SURFACE\([^;]+),\.T\.\);/, "$1,.F.);")
      .replace("DIRECTION('',(0,0,-1))", "DIRECTION('',(0,0,1))");
    const reversed = imported(reversedText);
    await matchesOcct(reversedText);
    const original = imported(text);
    expect(reversed[0].normals[2]).toBeCloseTo(original[0].normals[2]);
    expect(reversed[0].max).toEqual(original[0].max);
    const faces = imported(text.replace(/FACE_SURFACE\('',\((#\d+)\),#\d+,\.T\.\)/g, "FACE('',($1))"));
    expect(Array.from(faces[0].normals.slice(0, 3))).toEqual([0, 0, -1]);
  });

  it("falls back for reversed transform items and nonidentity source placements", () => {
    const swapped = text.replace(
      /ITEM_DEFINED_TRANSFORMATION\('','',#(\d+),#(\d+)\)/g,
      "ITEM_DEFINED_TRANSFORMATION('','',#$2,#$1)",
    );
    expect(parse(swapped)).toBeNull();
    const [, base] = match(/ITEM_DEFINED_TRANSFORMATION\('','',#(\d+),/);
    const shifted = text
      .replace(new RegExp(`(#${base}=AXIS2_PLACEMENT_3D\\('',)#\\d+`), "$1#900000001")
      .replace("ENDSEC;\nEND-ISO-10303-21;", "#900000001=CARTESIAN_POINT('',(2,3,4));\nENDSEC;\nEND-ISO-10303-21;");
    expect(parse(shifted)).toBeNull();
  });

  it("falls back for mixed surfaces, holes, polygons, unsupported styles, units, and missing references", () => {
    for (const unsupported of [
      text.replace("PLANE(", "CYLINDRICAL_SURFACE("),
      text.replace(/(FACE_SURFACE\([^;]+),\.T\.\);/, "$1,.F.);"),
      text.replace(/(FACE_OUTER_BOUND\([^;]+),\.T\.\);/, "$1,.F.);"),
      text.replace("FACE_OUTER_BOUND(", "FACE_BOUND("),
      text.replace(/POLY_LOOP\('',\((#[0-9]+),/, "POLY_LOOP('',($1,$1,"),
      text.replace(".BOTH.", ".POSITIVE."),
      text.replace("SI_UNIT(.MILLI.,.METRE.)", "CONVERSION_BASED_UNIT('inch',#999999999)"),
      text.replace(/FACETED_BREP\('tetra; ''sample''',#\d+\)/, "FACETED_BREP('broken',#999999999)"),
      text.replace("END-ISO-10303-21;", ""),
      text.replace(/^#\d+=CONTEXT_DEPENDENT_SHAPE_REPRESENTATION[^\n]+\n/m, ""),
      text.replace(/ITEM_DEFINED_TRANSFORMATION\('','',#\d+,/, "ITEM_DEFINED_TRANSFORMATION('','',#999999999,"),
    ])
      expect(parse(unsupported)).toBeNull();
  });

  it("keeps OCCT imports available for normal curved STEP files", async () => {
    const curved = readFileSync("node_modules/occt-import-js/test/testfiles/conical-surface/conical-surface.step");
    expect(readFacetedStep(curved)).toBeNull();
    const meshes = await importStep(curved);
    expect(meshes.length).toBeGreaterThan(0);
    expect(meshes.reduce((n, m) => n + (m.indices?.length ?? m.positions.length / 3) / 3, 0)).toBeGreaterThan(100);
  });
});
