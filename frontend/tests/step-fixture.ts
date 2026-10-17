// Generates colored faceted STEP tetrahedra with assembly placements for import tests and benchmarks.
export function createFacetedStep(parts: number, reusePart: boolean): Uint8Array<ArrayBuffer> {
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
      instances.push({ ...instances[0], placement: placement(point(i * 5, 0, 0), z, i % 2 ? y : x) });
      continue;
    }
    const vertices = [zero, point(1, 0, 0), point(0, 1, 0), point(0, 0, 1)];
    const faces: number[] = [];
    const specs = [
      { points: [0, 2, 1], normal: [0, 0, -1], tangent: [0, 1, 0] },
      { points: [0, 1, 3], normal: [0, -1, 0], tangent: [1, 0, 0] },
      { points: [0, 3, 2], normal: [-1, 0, 0], tangent: [0, 0, 1] },
      { points: [1, 2, 3], normal: [1, 1, 1], tangent: [-1, 1, 0] },
    ];
    for (const spec of specs) {
      const loop = emit(`POLY_LOOP('',(${spec.points.map((v) => `#${vertices[v]}`).join(",")}))`);
      const bound = emit(`FACE_OUTER_BOUND('',#${loop},.T.)`);
      const n = direction(spec.normal[0], spec.normal[1], spec.normal[2]);
      const t = direction(spec.tangent[0], spec.tangent[1], spec.tangent[2]);
      const frame = placement(vertices[spec.points[0]], n, t);
      const plane = emit(`PLANE('',#${frame})`);
      faces.push(emit(`FACE_SURFACE('',(#${bound}),#${plane},.T.)`));
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
