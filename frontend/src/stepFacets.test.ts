// Tests complete planar faceted STEP imports, polygon coverage, placements, colors, units, winding, and conservative fallback.
import { describe, it } from "node:test";
import { readFileSync } from "node:fs";
import { Vector3 } from "three";
import occtimportjs from "occt-import-js";
import { expect } from "@tests/expect";
import { createFacetedStep, createFramedStep, createPolygonStep } from "../tests/step-fixture";
import { FacetedStepReader } from "./stepFacets";
import { importStep } from "./stepImport";
import type { CadLoadProgress, CadProgress, StepImportTimings } from "./stepProtocol";

function input(bytes: Uint8Array) {
  const reader = new FacetedStepReader();
  reader.push(bytes);
  return reader;
}
function readFacetedStep(bytes: Uint8Array, timings?: StepImportTimings, progress?: CadProgress) {
  return input(bytes).meshes(timings, progress);
}

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

function canonicalTriples(values: ArrayLike<number>): string[] {
  const directions = new Set<string>();
  for (let i = 0; i < values.length; i += 3)
    directions.add(JSON.stringify([0, 1, 2].map((j) => Math.round(values[i + j] * 1e5) / 1e5 + 0)));
  return [...directions].sort();
}

function surfaceAreas(positions: ArrayLike<number>, indices: ArrayLike<number> | null) {
  const patches = new Map<string, number>();
  const count = indices?.length ?? positions.length / 3;
  const point = (i: number) => new Vector3().fromArray(positions, (indices ? indices[i] : i) * 3);
  for (let i = 0; i < count; i += 3) {
    const a = point(i),
      b = point(i + 1),
      c = point(i + 2);
    const cross = b.sub(a).cross(c.sub(a));
    const area = cross.length() / 2;
    expect(area).toBeGreaterThan(0);
    cross.normalize();
    const key = JSON.stringify([cross.x, cross.y, cross.z, cross.dot(a)].map((v) => Math.round(v * 1e4) / 1e4 + 0));
    patches.set(key, (patches.get(key) ?? 0) + area);
  }
  return patches;
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
    for (let j = 0; j < 3; j++) {
      expect((meshes[i].min[j] - min[j]) / Math.max(1, Math.abs(min[j]))).toBeCloseTo(0, 6);
      expect((meshes[i].max[j] - max[j]) / Math.max(1, Math.abs(max[j]))).toBeCloseTo(0, 6);
    }
    const patches = surfaceAreas(meshes[i].positions, meshes[i].indices);
    const expectedPatches = surfaceAreas(positions, expected.meshes[i].index?.array ?? null);
    expect([...patches.keys()].sort()).toEqual([...expectedPatches.keys()].sort());
    for (const [key, area] of expectedPatches) expect((patches.get(key) ?? 0) / area).toBeCloseTo(1, 5);
    const normals = expected.meshes[i].attributes.normal?.array;
    if (!normals) throw new Error("Missing OCCT normals");
    expect(canonicalTriples(meshes[i].normals)).toEqual(canonicalTriples(normals));
    const actualColor = meshes[i].color;
    const expectedColor = expected.meshes[i].color;
    if (!actualColor || !expectedColor) throw new Error("Missing STEP part color");
    for (let j = 0; j < 3; j++) expect(actualColor[j]).toBeCloseTo(expectedColor[j]);
  }
}

describe("faceted STEP", () => {
  it("indexes records before EOF across every lexical boundary and reports assembled face progress", () => {
    const updates: CadLoadProgress[] = [];
    const bytes = new TextEncoder().encode(
      new TextDecoder().decode(createFacetedStep(70, true)).replace("DATA;", "DATA;/* ; 'quotes' split */"),
    );
    const reader = new FacetedStepReader();
    for (let offset = 0; offset < bytes.length - 64; offset++) reader.push(bytes.subarray(offset, offset + 1));
    expect(reader.indexedRecords).toBeGreaterThan(100);
    expect(reader.byteLength).toBe(bytes.length - 64);
    reader.push(bytes.subarray(bytes.length - 64));
    const meshes = reader.meshes(undefined, (value) => updates.push(value));
    expect(meshes).toHaveLength(70);
    expect([...new Set(updates.map((value) => value.parseStep))]).toEqual(["geometry"]);
    const faces = updates.filter((value) => value.parseStep === "geometry");
    expect(faces[0].completed).toBe(0);
    expect(faces.at(-1)?.completed).toBe(280);
    expect(updates).toContainEqual({ stage: "parse", parseStep: "geometry", completed: 256, total: 280 });
  });

  it("resolves an entity spanning multiple decoded pages without flattening the file", () => {
    const source = new TextEncoder().encode(
      text.replace("tetra; ''sample''", "x".repeat(256 * 1024) + "é; ''sample''"),
    );
    const reader = new FacetedStepReader();
    for (let offset = 0; offset < source.length; offset += 1237) reader.push(source.subarray(offset, offset + 1237));
    const meshes = reader.meshes(undefined, undefined);
    expect(meshes).toHaveLength(2);
    expect(meshes?.[0].max).toEqual([1, 1, 1]);
  });

  it("counts shell faces without treating quoted names or comments as references", () => {
    const source = text.replaceAll(
      "CLOSED_SHELL('',(",
      "CLOSED_SHELL('shell #123123123; ''quoted''',(/* #999999999; ignored */",
    );
    const updates: CadLoadProgress[] = [];
    expect(readFacetedStep(new TextEncoder().encode(source), undefined, (p) => updates.push(p))).toHaveLength(2);
    expect(updates.at(-1)).toEqual({ stage: "parse", parseStep: "geometry", completed: 8, total: 8 });
    for (const malformed of [
      text.replace(/(CLOSED_SHELL\('',\([^)]+\))\)/, "$1,0)"),
      text.replace(/(CLOSED_SHELL\('',\()#[0-9]+/, "$1'face #123123123'"),
      text.replace("CLOSED_SHELL(", "FACETED_BREP("),
    ])
      expect(parse(malformed)).toBeNull();
  });

  it("resets determinate extraction to indeterminate kernel parsing on late unsupported geometry", async () => {
    const updates: CadLoadProgress[] = [];
    // A repeated closing vertex requires OCCT. Earlier bodies are supported;
    // progress must never publish their geometry as a result.
    const plain = new TextDecoder().decode(createFacetedStep(2, false));
    const loops = [...plain.matchAll(/POLY_LOOP\('',\(([^)]+)\)\)/g)];
    const last = loops.at(-1);
    if (!last) throw new Error("Missing final face loop");
    const points = last[1].split(",");
    const changed = plain.replace(last[0], `POLY_LOOP('',(${points.join(",")},${points[0]}))`);
    const meshes = await importStep(input(new TextEncoder().encode(changed)), undefined, (value) =>
      updates.push(value),
    );
    expect(meshes).toHaveLength(2);
    const extraction = updates.findIndex((value) => value.parseStep === "geometry");
    const init = updates.findIndex((value) => value.parseStep === "occt-init");
    const kernel = updates.findIndex((value) => value.parseStep === "occt");
    expect(extraction).toBeGreaterThanOrEqual(0);
    expect(init).toBeGreaterThan(extraction);
    expect(kernel).toBeGreaterThan(init);
    expect(updates[kernel]).toEqual({ stage: "parse", parseStep: "occt", completed: null, total: null });
    expect(updates.slice(extraction, init).every((value) => value.completed !== value.total)).toBe(true);
  });

  it("imports convex and concave closed polygon prisms with correct surface coverage", async () => {
    for (const contour of [
      [
        [0, 0],
        [2, 0],
        [2, 1],
        [0, 1],
      ],
      [
        [0, 0],
        [2, 0],
        [2, 1],
        [1, 1],
        [1, 2],
        [0, 2],
      ],
    ] as [number, number][][]) {
      for (const reversed of [false, true]) {
        const prism = createPolygonStep(contour, reversed, 1);
        await matchesOcct(prism);
        await matchesOcct(prism.replace(".MILLI.,.METRE.", "$,.METRE."));
        const rotated = prism.replace(
          /^#1[0-9]{7}=(CARTESIAN_POINT|DIRECTION)\('',\(([^)]+)\)\);$/gm,
          (record: string, _kind: string, coordinates: string) => {
            const [x, y, z] = coordinates.split(",").map(Number);
            // A tilted right-handed frame exercises projection and winding on
            // non-axis-aligned planes while assembly placements stay unchanged.
            const c = Math.SQRT1_2;
            return record.replace(coordinates, [z, (x - y) * c, (x + y) * c].join(","));
          },
        );
        await matchesOcct(rotated);
        // Removing plane metadata still leaves an explicitly oriented FACE.
        const plain = imported(prism.replace(/FACE_SURFACE\(''?,\((#[0-9]+)\),#[0-9]+,\.[TF]\.\)/g, "FACE('',($1))"));
        expect(surfaceAreas(plain[0].positions, null)).toEqual(surfaceAreas(imported(prism)[0].positions, null));
      }
    }
    const mesh = imported(
      createPolygonStep(
        [
          [0, 0],
          [2, 0],
          [2, 1],
          [1, 1],
          [1, 2],
          [0, 2],
        ],
        false,
        1,
      ),
    )[0];
    expect(mesh.positions.length / 9).toBe(20);
    for (let i = 0; i < mesh.positions.length; i += 9) {
      const v = mesh.positions.slice(i, i + 9);
      const x = (v[0] + v[3] + v[6]) / 3,
        y = (v[1] + v[4] + v[7]) / 3;
      // No triangle may fill the missing upper-right quadrant of the L.
      expect(x <= 1 + 1e-7 || y <= 1 + 1e-7).toBe(true);
      const cross = new Vector3(v[3] - v[0], v[4] - v[1], v[5] - v[2])
        .cross(new Vector3(v[6] - v[0], v[7] - v[1], v[8] - v[2]))
        .normalize();
      expect(cross.dot(new Vector3().fromArray(mesh.normals, i))).toBeCloseTo(1, 6);
    }
  });

  it("rejects ambiguous, intersecting, degenerate, nonplanar, or excessively large polygon loops wholesale", () => {
    const rectangle = createPolygonStep(
      [
        [0, 0],
        [2, 0],
        [2, 1],
        [0, 1],
      ],
      false,
      1,
    );
    const invalid = [
      createPolygonStep(
        [
          [0, 0],
          [2, 2],
          [0, 2],
          [2, 0],
        ],
        false,
        1,
      ), // Bowtie.
      createPolygonStep(
        [
          [0, 0],
          [3, 0],
          [3, 2],
          [1, 0],
          [0, 2],
        ],
        false,
        1,
      ), // Nonadjacent vertex on edge.
      createPolygonStep(
        [
          [0, 0],
          [3, 0],
          [3, 2],
          [1, 2],
          [1, 0],
          [2, 0],
          [2, 1],
          [0, 1],
        ],
        false,
        1,
      ), // Overlap.
      createPolygonStep(
        [
          [0, 0],
          [2, 0],
          [2, 0],
          [0, 1],
        ],
        false,
        1,
      ),
      createPolygonStep(
        [
          [0, 0],
          [1, 0],
          [2, 0],
          [2, 1],
          [0, 1],
        ],
        false,
        1,
      ), // Collinear corner.
      createPolygonStep(
        [
          [0, 0],
          [1, 0],
          [2, 0],
          [3, 0],
        ],
        false,
        1,
      ),
      createPolygonStep(
        [
          [0, 0],
          [2, 0],
          [2, 1],
          [0, 1],
          [0, 0],
        ],
        false,
        1,
      ),
      createPolygonStep(
        [
          [0, 0],
          [2, 0],
          [2, 1e-12],
          [0, 1e-12],
        ],
        false,
        1,
      ),
      rectangle.replace("CARTESIAN_POINT('',(2,1,0))", "CARTESIAN_POINT('',(2,1,0.1))"),
      rectangle.replaceAll(/(FACE_SURFACE\(''?,\(#[0-9]+)(\),)/g, "$1,#999999999$2"), // Hole.
      createPolygonStep(
        Array.from({ length: 257 }, (_, i) => [Math.cos((i / 257) * Math.PI * 2), Math.sin((i / 257) * Math.PI * 2)]),
        false,
        1,
      ),
    ];
    for (const value of invalid) expect(parse(value)).toBeNull();
    // Keep a valid first body, so unsupported later geometry cannot yield a partial import.
    const two = new TextDecoder().decode(createFacetedStep(2, false));
    const loop = [...two.matchAll(/POLY_LOOP\('',\(([^)]+)\)\)/g)].at(-1);
    if (!loop) throw new Error("Missing final loop");
    expect(parse(two.replace(loop[0], `POLY_LOOP('',(${loop[1]},${loop[1].split(",")[0]}))`))).toBeNull();
  });

  it("bounds polygon validation across reused parts and reports faces rather than generated triangles", () => {
    const updates: CadLoadProgress[] = [];
    const prism = createPolygonStep(
      [
        [0, 0],
        [2, 0],
        [2, 1],
        [1, 1],
        [1, 2],
        [0, 2],
      ],
      false,
      3,
    );
    const meshes = readFacetedStep(new TextEncoder().encode(prism), undefined, (p) => updates.push(p));
    expect(meshes).toHaveLength(3);
    expect(meshes?.map((m) => m.positions.length / 9)).toEqual([20, 20, 20]);
    expect(updates.at(-1)).toEqual({ stage: "parse", parseStep: "geometry", completed: 24, total: 24 });
    const circle = Array.from({ length: 256 }, (_, i): [number, number] => [
      Math.cos((i / 256) * Math.PI * 2),
      Math.sin((i / 256) * Math.PI * 2),
    ]);
    expect(parse(createPolygonStep(circle, false, 1))).toHaveLength(1);
    expect(parse(createPolygonStep(circle, false, 32))).toBeNull();
  });

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

  it("resolves nonidentity rigid frames and independently reversed items and representations like OCCT", async () => {
    const occt = await occtimportjs();
    for (const nested of [false, true]) {
      for (const reverseItems of [false, true]) {
        for (const reverseRepresentations of [false, true]) {
          let source = createFramedStep(nested ? 3 : 2, nested, nested);
          if (reverseItems)
            source = source.replace(
              /ITEM_DEFINED_TRANSFORMATION\('','',#(\d+),#(\d+)\)/g,
              "ITEM_DEFINED_TRANSFORMATION('','',#$2,#$1)",
            );
          if (reverseRepresentations)
            source = source.replace(
              /REPRESENTATION_RELATIONSHIP\('','',#(\d+),#(\d+)\)/g,
              "REPRESENTATION_RELATIONSHIP('','',#$2,#$1)",
            );
          const actual = imported(source);
          const expected = occt.ReadStepFile(new TextEncoder().encode(source), null);
          expect(expected.success).toBe(true);
          const canonical = (positions: ArrayLike<number>, normals: ArrayLike<number>, color: number[] | null) => ({
            vertices: canonicalTriples(positions),
            normals: canonicalTriples(normals),
            color,
          });
          const actualGeometry = actual.map((m) => canonical(m.positions, m.normals, m.color));
          const expectedGeometry = expected.meshes.map((m) => {
            const normals = m.attributes.normal?.array;
            if (!normals) throw new Error("Missing OCCT assembly normals");
            return canonical(m.attributes.position.array, normals, m.color ?? null);
          });
          const byVertices = (a: { vertices: string[] }, b: { vertices: string[] }) =>
            a.vertices.join(";").localeCompare(b.vertices.join(";"));
          actualGeometry.sort(byVertices);
          expectedGeometry.sort(byVertices);
          expect(actualGeometry.map((m) => m.vertices)).toEqual(expectedGeometry.map((m) => m.vertices));
          expect(actualGeometry.map((m) => m.normals)).toEqual(expectedGeometry.map((m) => m.normals));
          for (let i = 0; i < actualGeometry.length; i++) {
            const color = actualGeometry[i].color;
            const expectedColor = expectedGeometry[i].color;
            if (!color || !expectedColor) throw new Error("Missing assembly color");
            for (let j = 0; j < 3; j++) expect(color[j]).toBeCloseTo(expectedColor[j], 6);
          }
          // Literal bounds make incorrect multiplication order independently visible.
          expect(actual.map((m) => [m.min, m.max])).toEqual(
            nested
              ? [
                  [
                    [6, 12, -10],
                    [7, 13, -9],
                  ],
                  [
                    [6, 12, -6],
                    [7, 13, -5],
                  ],
                  [
                    [6, 12, 0],
                    [7, 13, 1],
                  ],
                ]
              : [
                  [
                    [-3, 1, -4],
                    [-2, 2, -3],
                  ],
                  [
                    [-3, -3, -4],
                    [-2, -2, -3],
                  ],
                ],
          );
        }
      }
    }
  });

  it("falls back wholesale for ambiguous ownership, unrelated products, and unsupported or degenerate frames", () => {
    const [, source, target] = match(/ITEM_DEFINED_TRANSFORMATION\('','',#(\d+),#(\d+)\)/);
    const [, targetOrigin, targetZ] = match(new RegExp(`#${target}=AXIS2_PLACEMENT_3D\\('',#(\\d+),#(\\d+),#\\d+`));
    const ambiguous = text.replace(/(FACETED_BREP_SHAPE_REPRESENTATION\('',\([^)]*)\)/, `$1,#${target})`);
    const missingOwnership = text
      .replaceAll(`#${source},`, "#900000001,")
      .replace(
        "ENDSEC;\nEND-ISO-10303-21;",
        `#900000001=AXIS2_PLACEMENT_3D('',#${targetOrigin},#${targetZ},$);\nENDSEC;\nEND-ISO-10303-21;`,
      )
      .replace(/ITEM_DEFINED_TRANSFORMATION\('','',#900000001,/, `ITEM_DEFINED_TRANSFORMATION('','',#${source},`);
    for (const invalid of [
      ambiguous,
      ambiguous.replace(
        /ITEM_DEFINED_TRANSFORMATION\('','',#(\d+),#(\d+)\)/g,
        "ITEM_DEFINED_TRANSFORMATION('','',#$2,#$1)",
      ),
      missingOwnership,
      text.replace(/(NEXT_ASSEMBLY_USAGE_OCCURRENCE\('[^']*','[^']*','',)#\d+,/, "$1#999999999,"),
      text.replace(/ITEM_DEFINED_TRANSFORMATION\('','',#\d+,/, `ITEM_DEFINED_TRANSFORMATION('','',#${targetOrigin},`),
      text.replace(new RegExp(`(#${source}=AXIS2_PLACEMENT_3D\\('',#\\d+,#\\d+,)#\\d+`), `$1#${targetZ}`),
      text.replace("DIRECTION('',(1,0,0))", "DIRECTION('',(0,0,0))"),
      text.replace("DIRECTION('',(1,0,0))", "DIRECTION('',(1E308,0,0))"),
      text.replace("ITEM_DEFINED_TRANSFORMATION(", "CARTESIAN_TRANSFORMATION_OPERATOR_3D("),
    ])
      expect(parse(invalid)).toBeNull();
  });

  it("falls back for mixed surfaces, holes, duplicate vertices, unsupported styles, units, and missing references", () => {
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
    const meshes = await importStep(input(curved));
    expect(meshes.length).toBeGreaterThan(0);
    expect(meshes.reduce((n, m) => n + (m.indices?.length ?? m.positions.length / 3) / 3, 0)).toBeGreaterThan(100);
  });
});
