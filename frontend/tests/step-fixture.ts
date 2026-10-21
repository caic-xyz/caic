// Generates faceted STEP assemblies, dense tetrahedra, rigid frames, and polygon prisms for import checks.
import assert from "node:assert/strict";
import { Vector3 } from "three";
export function createFacetedStep(parts: number, reusePart: boolean): Uint8Array<ArrayBuffer> {
  return createAssembly(parts, reusePart, 1);
}

export function createDenseFacetedStep(subdivisions: number): Uint8Array<ArrayBuffer> {
  return createAssembly(1, false, subdivisions);
}

function createAssembly(parts: number, reusePart: boolean, subdivisions: number): Uint8Array<ArrayBuffer> {
  const records: string[] = [];
  function emit(body: string): number {
    records.push(`#${records.length + 1}=${body};`);
    return records.length;
  }
  function point(x: number, y: number, z: number) {
    return emit(`CARTESIAN_POINT('',(${x},${y},${z}))`);
  }
  function direction(x: number, y: number, z: number) {
    return emit(`DIRECTION('',(${x},${y},${z}))`);
  }
  function placement(origin: number, z: number, x: number) {
    return emit(`AXIS2_PLACEMENT_3D('',#${origin},#${z},#${x})`);
  }
  const app = emit("APPLICATION_CONTEXT('automotive_design')");
  emit(`APPLICATION_PROTOCOL_DEFINITION('international standard','automotive_design',2000,#${app})`);
  const length = emit("(LENGTH_UNIT()NAMED_UNIT(*)SI_UNIT(.MILLI.,.METRE.))");
  const angle = emit("(NAMED_UNIT(*)PLANE_ANGLE_UNIT()SI_UNIT($,.RADIAN.))");
  const solidAngle = emit("(NAMED_UNIT(*)SI_UNIT($,.STERADIAN.)SOLID_ANGLE_UNIT())");
  const context = emit(
    `(GEOMETRIC_REPRESENTATION_CONTEXT(3)GLOBAL_UNIT_ASSIGNED_CONTEXT((#${length},#${angle},#${solidAngle}))REPRESENTATION_CONTEXT('',''))`,
  );
  const zero = point(0, 0, 0);
  const z = direction(0, 0, 1);
  const x = direction(1, 0, 0);
  const y = direction(0, 1, 0);
  const base = placement(zero, z, x);
  function product(name: string) {
    const ctx = emit(`PRODUCT_CONTEXT('',#${app},'mechanical')`);
    const p = emit(`PRODUCT('${name}','${name}','',(#${ctx}))`);
    const formation = emit(`PRODUCT_DEFINITION_FORMATION('','',#${p})`);
    const definition = emit(`PRODUCT_DEFINITION_CONTEXT('part definition',#${app},'design')`);
    return emit(`PRODUCT_DEFINITION('design','',#${formation},#${definition})`);
  }
  function shape(productId: number) {
    return emit(`PRODUCT_DEFINITION_SHAPE('','',#${productId})`);
  }
  const assembly = product("assembly");
  const instances: { representation: number; product: number; placement: number }[] = [];
  for (let i = 0; i < parts; i++) {
    if (reusePart && i > 0) {
      assert.ok(instances[0] !== undefined);
      instances.push({ ...instances[0], placement: placement(point(i * 5, 0, 0), z, i % 2 ? y : x) });
      continue;
    }
    const coordinates = [
      [0, 0, 0],
      [1, 0, 0],
      [0, 1, 0],
      [0, 0, 1],
    ] as const;
    const vertices = [zero, point(1, 0, 0), point(0, 1, 0), point(0, 0, 1)] as const;
    const faces: number[] = [];
    const specs = [
      { points: [0, 2, 1], normal: [0, 0, -1], tangent: [0, 1, 0] },
      { points: [0, 1, 3], normal: [0, -1, 0], tangent: [1, 0, 0] },
      { points: [0, 3, 2], normal: [-1, 0, 0], tangent: [0, 0, 1] },
      { points: [1, 2, 3], normal: [1, 1, 1], tangent: [-1, 1, 0] },
    ] as const;
    for (const spec of specs) {
      const n = direction(spec.normal[0], spec.normal[1], spec.normal[2]);
      const t = direction(spec.tangent[0], spec.tangent[1], spec.tangent[2]);
      const frame = placement(vertices[spec.points[0]], n, t);
      const plane = emit(`PLANE('',#${frame})`);
      const triangle = (points: number[]) => {
        const loop = emit(`POLY_LOOP('',(${points.map((v) => `#${v}`).join(",")}))`);
        const bound = emit(`FACE_OUTER_BOUND('',#${loop},.T.)`);
        faces.push(emit(`FACE_SURFACE('',(#${bound}),#${plane},.T.)`));
      };
      if (subdivisions === 1) {
        triangle(spec.points.map((v) => vertices[v]));
        continue;
      }
      const a = new Vector3(...coordinates[spec.points[0]]);
      const b = new Vector3(...coordinates[spec.points[1]]).sub(a);
      const c = new Vector3(...coordinates[spec.points[2]]).sub(a);
      const rows: number[][] = [];
      for (let row = 0; row <= subdivisions; row++) {
        const points: number[] = [];
        for (let column = 0; column <= subdivisions - row; column++) {
          const position = a
            .clone()
            .addScaledVector(b, row / subdivisions)
            .addScaledVector(c, column / subdivisions);
          points.push(point(position.x, position.y, position.z));
        }
        rows.push(points);
      }
      for (let row = 0; row < subdivisions; row++) {
        for (let column = 0; column < subdivisions - row; column++) {
          const top = rows[row],
            bottom = rows[row + 1];
          assert.ok(top !== undefined && bottom !== undefined);
          const a = top[column],
            b = bottom[column],
            c = top[column + 1];
          assert.ok(a !== undefined && b !== undefined && c !== undefined);
          triangle([a, b, c]);
          if (column < subdivisions - row - 1) {
            const d = bottom[column + 1];
            assert.ok(d !== undefined);
            triangle([b, d, c]);
          }
        }
      }
    }
    const shell = emit(`CLOSED_SHELL('',(${faces.map((f) => `#${f}`).join(",")}))`);
    const body = emit(`FACETED_BREP('tetra; ''sample''',#${shell})`);
    const representation = emit(`FACETED_BREP_SHAPE_REPRESENTATION('',(#${base},#${body}),#${context})`);
    const definition = product(`part_${i}`);
    emit(`SHAPE_DEFINITION_REPRESENTATION(#${shape(definition)},#${representation})`);
    const color = emit(`COLOUR_RGB('',${i % 2 ? "0.8,0.1,0.3" : "0.2,0.4,0.6"})`);
    const fillColor = emit(`FILL_AREA_STYLE_COLOUR('',#${color})`);
    const fill = emit(`FILL_AREA_STYLE('',(#${fillColor}))`);
    const surface = emit(`SURFACE_STYLE_FILL_AREA(#${fill})`);
    const side = emit(`SURFACE_SIDE_STYLE('',(#${surface}))`);
    const usage = emit(`SURFACE_STYLE_USAGE(.BOTH.,#${side})`);
    const style = emit(`PRESENTATION_STYLE_ASSIGNMENT((#${usage}))`);
    emit(`STYLED_ITEM('',(#${style}),#${body})`);
    instances.push({ representation, product: definition, placement: placement(point(i * 5, 0, 0), z, i % 2 ? y : x) });
  }
  const root = emit(
    `SHAPE_REPRESENTATION('',(#${base},${instances.map((v) => `#${v.placement}`).join(",")}),#${context})`,
  );
  emit(`SHAPE_DEFINITION_REPRESENTATION(#${shape(assembly)},#${root})`);
  for (const [i, instance] of instances.entries()) {
    const transform = emit(`ITEM_DEFINED_TRANSFORMATION('','',#${base},#${instance.placement})`);
    const relationship = emit(
      `(REPRESENTATION_RELATIONSHIP('','',#${instance.representation},#${root})REPRESENTATION_RELATIONSHIP_WITH_TRANSFORMATION(#${transform})SHAPE_REPRESENTATION_RELATIONSHIP())`,
    );
    const occurrence = emit(
      `NEXT_ASSEMBLY_USAGE_OCCURRENCE('${i}','part_${i}','',#${assembly},#${instance.product},$)`,
    );
    emit(`CONTEXT_DEPENDENT_SHAPE_REPRESENTATION(#${relationship},#${shape(occurrence)})`);
  }
  return new TextEncoder().encode(`ISO-10303-21;
HEADER;
FILE_DESCRIPTION(('Faceted assembly'),'2;1');
FILE_NAME('','1970-01-01T00:00:00',(''),(''),'fixture','fixture','');
FILE_SCHEMA(('AUTOMOTIVE_DESIGN'));
ENDSEC;
DATA;
${records.join("\n")}
ENDSEC;
END-ISO-10303-21;
`);
}

// Closed prisms exercise polygon coverage without relying on a particular diagonal.
export function createPolygonStep(contour: readonly [number, number][], reversed: boolean, instances: number): string {
  const source = new TextDecoder().decode(createFacetedStep(instances, true));
  const records: string[] = [];
  const emit = (entity: string) => {
    const id = 10000000 + records.length;
    records.push(`#${id}=${entity};`);
    return id;
  };
  const coordinates = [0, 1].flatMap((z) => contour.map(([x, y]): [number, number, number] => [x, y, z]));
  const points = coordinates.map((p) => emit(`CARTESIAN_POINT('',(${p.join(",")}))`));
  const n = contour.length;
  const loops = [Array.from({ length: n }, (_, i) => n - i - 1), Array.from({ length: n }, (_, i) => n + i)];
  for (let i = 0; i < n; i++) loops.push([i, (i + 1) % n, ((i + 1) % n) + n, i + n]);
  const faces = loops.map((indices) => {
    const [a, b, c] = indices.map((i) => coordinates[i]);
    assert.ok(a !== undefined && b !== undefined && c !== undefined);
    const ab = new Vector3(...b).sub(new Vector3(...a));
    const ac = new Vector3(...c).sub(new Vector3(...a));
    const normal = ab.clone().cross(ac).toArray();
    const z = emit(`DIRECTION('',(${normal.map((v) => (reversed ? -v : v)).join(",")}))`);
    const x = emit(`DIRECTION('',(${ab.toArray().join(",")}))`);
    assert.ok(indices[0] !== undefined);
    const placement = emit(`AXIS2_PLACEMENT_3D('',#${points[indices[0]]},#${z},#${x})`);
    const plane = emit(`PLANE('',#${placement})`);
    const loop = emit(`POLY_LOOP('',(${indices.map((i) => `#${points[i]}`).join(",")}))`);
    const bound = emit(`FACE_OUTER_BOUND('',#${loop},.T.)`);
    return emit(`FACE_SURFACE('',(#${bound}),#${plane},.${reversed ? "F" : "T"}.)`);
  });
  return source
    .replace(/CLOSED_SHELL\('',\([^)]+\)\)/, `CLOSED_SHELL('',(${faces.map((id) => `#${id}`).join(",")}))`)
    .replace("ENDSEC;\nEND-ISO-10303-21;", `${records.join("\n")}\nENDSEC;\nEND-ISO-10303-21;`);
}

// Noncommuting frames expose the difference between S^-1*T and T*S^-1.
// The wrapper rotates around X and Y while reused parts rotate around Z.
export function createFramedStep(parts: number, reusePart: boolean, nested: boolean): string {
  const source = new TextDecoder().decode(createFacetedStep(parts, reusePart));
  function match(pattern: RegExp) {
    const result = source.match(pattern);
    if (!result) throw new Error("Missing STEP assembly fixture entity");
    return result;
  }
  const [, base] = match(/ITEM_DEFINED_TRANSFORMATION\('','',#(\d+),/);
  const [, z, x] = match(new RegExp(`#${base}=AXIS2_PLACEMENT_3D\\('',#\\d+,#(\\d+),#(\\d+)\\)`));
  const [, y] = match(/#(\d+)=DIRECTION\('',\(0,1,0\)\)/);
  const [, root, items, context] = match(/^#(\d+)=SHAPE_REPRESENTATION\('',\(([^\n]+)\),#(\d+)\);$/m);
  const [, product] = match(/NEXT_ASSEMBLY_USAGE_OCCURRENCE\('[^']*','[^']*','',#(\d+),/);
  const [, app] = match(/PRODUCT_CONTEXT\('',#(\d+),/);
  let records = "#900000001=CARTESIAN_POINT('',(2,3,4));\n";
  if (nested)
    records += `
#900000002=PRODUCT_CONTEXT('',#${app},'mechanical');
#900000003=PRODUCT('wrapper','wrapper','',(#900000002));
#900000004=PRODUCT_DEFINITION_FORMATION('','',#900000003);
#900000005=PRODUCT_DEFINITION_CONTEXT('part definition',#${app},'design');
#900000006=PRODUCT_DEFINITION('design','',#900000004,#900000005);
#900000007=PRODUCT_DEFINITION_SHAPE('','',#900000006);
#900000008=CARTESIAN_POINT('',(7,11,13));
#900000009=DIRECTION('',(0,-1,0));
#900000010=AXIS2_PLACEMENT_3D('',#900000008,#900000009,#${x});
#900000011=CARTESIAN_POINT('',(17,19,23));
#900000012=DIRECTION('',(0,0,-1));
#900000013=AXIS2_PLACEMENT_3D('',#900000011,#${x},#900000012);
#900000014=SHAPE_REPRESENTATION('',(#900000013),#${context});
#900000015=SHAPE_DEFINITION_REPRESENTATION(#900000007,#900000014);
#900000016=ITEM_DEFINED_TRANSFORMATION('','',#900000010,#900000013);
#900000017=(REPRESENTATION_RELATIONSHIP('','',#${root},#900000014)REPRESENTATION_RELATIONSHIP_WITH_TRANSFORMATION(#900000016)SHAPE_REPRESENTATION_RELATIONSHIP());
#900000018=NEXT_ASSEMBLY_USAGE_OCCURRENCE('wrapper','','',#900000006,#${product},$);
#900000019=PRODUCT_DEFINITION_SHAPE('','',#900000018);
#900000020=CONTEXT_DEPENDENT_SHAPE_REPRESENTATION(#900000017,#900000019);
`;
  let framed = source.replace(
    new RegExp(`(#${base}=AXIS2_PLACEMENT_3D\\('',)#\\d+,#\\d+,#\\d+`),
    `$1#900000001,#${z},#${y}`,
  );
  if (nested)
    framed = framed.replace(
      `#${root}=SHAPE_REPRESENTATION('',(${items}),#${context});`,
      `#${root}=SHAPE_REPRESENTATION('',(${items},#900000010),#${context});`,
    );
  return framed.replace("ENDSEC;\nEND-ISO-10303-21;", `${records}ENDSEC;\nEND-ISO-10303-21;`);
}
