// Reads complete triangular planar STEP assemblies directly; unsupported representations use the OCCT importer.
import { Color, Matrix4, SRGBColorSpace, Vector3 } from "three";
import { createStepMesh } from "./stepMesh";
import type { StepMesh } from "./stepProtocol";

// Only this closed subset can bypass OCCT. In particular, curves, holes,
// mapped representations, face styles, and alternate surface types must never
// result in a partial preview. Unknown entities opt the entire file out.
const supported: Record<string, number> = {
  APPLICATION_CONTEXT: 1,
  APPLICATION_PROTOCOL_DEFINITION: 4,
  AXIS2_PLACEMENT_3D: 4,
  CARTESIAN_POINT: 2,
  CLOSED_SHELL: 2,
  COLOUR_RGB: 4,
  CONTEXT_DEPENDENT_SHAPE_REPRESENTATION: 2,
  DIRECTION: 2,
  FACE: 2,
  FACE_OUTER_BOUND: 3,
  FACE_SURFACE: 4,
  FACETED_BREP: 2,
  FACETED_BREP_SHAPE_REPRESENTATION: 3,
  FILL_AREA_STYLE: 2,
  FILL_AREA_STYLE_COLOUR: 2,
  ITEM_DEFINED_TRANSFORMATION: 4,
  MECHANICAL_DESIGN_GEOMETRIC_PRESENTATION_REPRESENTATION: 3,
  NEXT_ASSEMBLY_USAGE_OCCURRENCE: 6,
  PLANE: 2,
  POLY_LOOP: 2,
  PRESENTATION_STYLE_ASSIGNMENT: 1,
  PRODUCT: 4,
  PRODUCT_CONTEXT: 3,
  PRODUCT_DEFINITION: 4,
  PRODUCT_DEFINITION_CONTEXT: 3,
  PRODUCT_DEFINITION_FORMATION: 3,
  PRODUCT_DEFINITION_SHAPE: 3,
  PRODUCT_RELATED_PRODUCT_CATEGORY: 3,
  SHAPE_DEFINITION_REPRESENTATION: 2,
  SHAPE_REPRESENTATION: 3,
  STYLED_ITEM: 3,
  SURFACE_SIDE_STYLE: 2,
  SURFACE_STYLE_FILL_AREA: 1,
  SURFACE_STYLE_USAGE: 2,
  UNCERTAINTY_MEASURE_WITH_UNIT: 4,
};
const compoundTypes: Record<string, number> = {
  GEOMETRIC_REPRESENTATION_CONTEXT: 1,
  GLOBAL_UNCERTAINTY_ASSIGNED_CONTEXT: 1,
  GLOBAL_UNIT_ASSIGNED_CONTEXT: 1,
  LENGTH_UNIT: 0,
  NAMED_UNIT: 1,
  PLANE_ANGLE_UNIT: 0,
  REPRESENTATION_CONTEXT: 2,
  REPRESENTATION_RELATIONSHIP: 4,
  REPRESENTATION_RELATIONSHIP_WITH_TRANSFORMATION: 1,
  SHAPE_REPRESENTATION_RELATIONSHIP: 0,
  SI_UNIT: 2,
  SOLID_ANGLE_UNIT: 0,
};
const geometryTypes = new Set([
  "AXIS2_PLACEMENT_3D",
  "CARTESIAN_POINT",
  "DIRECTION",
  "FACE",
  "FACE_OUTER_BOUND",
  "FACE_SURFACE",
  "PLANE",
  "POLY_LOOP",
]);

class Unsupported extends Error {}
function requireSupported(condition: unknown): asserts condition {
  if (!condition) throw new Unsupported();
}

interface Entity {
  name: string;
  args: Value[];
}
type Value = number | string | null | { ref: number } | { enum: string } | Value[] | Entity;

function reference(value: Value | undefined): number {
  requireSupported(value && typeof value === "object" && "ref" in value);
  return value.ref;
}
function list(value: Value | undefined): Value[] {
  requireSupported(Array.isArray(value));
  return value;
}
function number(value: Value | undefined): number {
  requireSupported(typeof value === "number" && Number.isFinite(value));
  return value;
}
function enumeration(value: Value | undefined): string {
  requireSupported(value && typeof value === "object" && "enum" in value);
  return value.enum;
}
function flag(value: Value | undefined): boolean {
  const text = enumeration(value);
  requireSupported(text === "T" || text === "F");
  return text === "T";
}

// STEP strings escape quotes by doubling them. Semicolons inside strings or
// comments are not record boundaries. Scan bytes to avoid a second 512 MiB
// whole-file string; decode only individual entities when they are needed.
function scan(bytes: Uint8Array, visit: (id: number, start: number, end: number) => void) {
  let start = 0;
  let quoted = false;
  let comment = false;
  let section = 0;
  const decoder = new TextDecoder();
  for (let i = 0; i < bytes.length; i++) {
    const c = bytes[i];
    if (comment) {
      if (c === 42 && bytes[i + 1] === 47) {
        comment = false;
        i++;
      }
    } else if (quoted) {
      if (c === 39) {
        if (bytes[i + 1] === 39) i++;
        else quoted = false;
      }
    } else if (c === 47 && bytes[i + 1] === 42) {
      comment = true;
      i++;
    } else if (c === 39) quoted = true;
    else if (c === 59) {
      let p = start;
      while (p < i) {
        if (bytes[p] <= 32) p++;
        else if (bytes[p] === 47 && bytes[p + 1] === 42) {
          p += 2;
          while (p < i && !(bytes[p] === 42 && bytes[p + 1] === 47)) p++;
          p += 2;
        } else break;
      }
      if (bytes[p] === 35) {
        requireSupported(section === 4);
        let id = 0;
        p++;
        const digits = p;
        while (bytes[p] >= 48 && bytes[p] <= 57) id = id * 10 + bytes[p++] - 48;
        requireSupported(p > digits && id > 0 && id <= 0xffffffff);
        while (bytes[p] <= 32) p++;
        requireSupported(bytes[p++] === 61);
        visit(id, p, i);
      } else {
        const statement = decoder.decode(bytes.subarray(p, i)).trim();
        if (section === 0 && statement === "ISO-10303-21") section = 1;
        else if (section === 1 && statement === "HEADER") section = 2;
        else if (section === 2 && statement === "ENDSEC") section = 3;
        else if (section === 3 && statement === "DATA") section = 4;
        else if (section === 4 && statement === "ENDSEC") section = 5;
        else if (section === 5 && statement === "END-ISO-10303-21") section = 6;
        else {
          requireSupported(section === 2);
          const header = new Arguments(statement).read();
          requireSupported(
            header.length === 1 && ["FILE_DESCRIPTION", "FILE_NAME", "FILE_SCHEMA"].includes(header[0].name),
          );
        }
      }
      start = i + 1;
    }
  }
  requireSupported(!quoted && !comment && section === 6);
  for (let i = start; i < bytes.length; i++) requireSupported(bytes[i] <= 32);
}

class Arguments {
  private offset = 0;
  private readonly token =
    /\s+|\/\*[\s\S]*?\*\/|'(?:[^']|'')*'|#[0-9]+|\.[A-Z_]+\.|[-+]?(?:\d+\.?\d*|\.\d+)(?:[Ee][-+]?\d+)?|[A-Z_][A-Z_0-9]*|[(),$*]/y;
  constructor(private readonly text: string) {}
  private next(): string {
    while (true) {
      this.token.lastIndex = this.offset;
      const match = this.token.exec(this.text);
      requireSupported(match);
      this.offset = this.token.lastIndex;
      if (!/^\s|^\/\*/.test(match[0])) return match[0];
    }
  }
  private expect(token: string) {
    requireSupported(this.next() === token);
  }
  private values(): Value[] {
    const result: Value[] = [];
    let token = this.next();
    if (token === ")") return result;
    while (true) {
      result.push(this.value(token));
      token = this.next();
      if (token === ")") return result;
      requireSupported(token === ",");
      token = this.next();
    }
  }
  private value(token: string): Value {
    if (token === "(") return this.values();
    if (token === "$" || token === "*") return null;
    if (token.startsWith("'")) return token.slice(1, -1).replaceAll("''", "'");
    if (token.startsWith("#")) return { ref: Number(token.slice(1)) };
    if (token.startsWith(".") && token.endsWith(".")) return { enum: token.slice(1, -1) };
    if (/^[A-Z_]/.test(token)) {
      this.expect("(");
      return { name: token, args: this.values() };
    }
    const value = Number(token);
    requireSupported(Number.isFinite(value));
    return value;
  }
  read(): Entity[] {
    const first = this.next();
    const entities: Entity[] = [];
    if (first === "(") {
      let token = this.next();
      while (token !== ")") {
        const entity = this.value(token);
        requireSupported(entity && typeof entity === "object" && "name" in entity);
        entities.push(entity);
        token = this.next();
      }
    } else {
      const entity = this.value(first);
      requireSupported(entity && typeof entity === "object" && "name" in entity);
      entities.push(entity);
    }
    requireSupported(this.text.slice(this.offset).trim() === "");
    return entities;
  }
}

class Records {
  private readonly decoder = new TextDecoder();
  private readonly ids: Uint32Array;
  private readonly starts: Uint32Array;
  private readonly ends: Uint32Array;
  private readonly order: Uint32Array | null;
  readonly metadata = new Map<string, number[]>();
  constructor(private readonly bytes: Uint8Array) {
    let count = 0;
    scan(bytes, (id, start, end) => {
      const head = this.decoder.decode(bytes.subarray(start, Math.min(start + 128, end)));
      const match = /^\s*([A-Z_][A-Z_0-9]*|\()/.exec(head);
      requireSupported(match);
      const name = match[1] === "(" ? "" : match[1];
      requireSupported(name === "" || Object.hasOwn(supported, name));
      if (!geometryTypes.has(name)) {
        const entries = this.metadata.get(name);
        if (entries) entries.push(id);
        else this.metadata.set(name, [id]);
      }
      count++;
    });
    requireSupported(this.metadata.has("FACETED_BREP"));
    this.ids = new Uint32Array(count);
    this.starts = new Uint32Array(count);
    this.ends = new Uint32Array(count);
    let index = 0;
    let sorted = true;
    scan(bytes, (id, start, end) => {
      if (index && id <= this.ids[index - 1]) sorted = false;
      this.ids[index] = id;
      this.starts[index] = start;
      this.ends[index++] = end;
    });
    this.order = sorted
      ? null
      : Uint32Array.from({ length: count }, (_, i) => i).sort((a, b) => this.ids[a] - this.ids[b]);
    for (let i = 1; i < count; i++) {
      requireSupported(this.ids[this.order?.[i - 1] ?? i - 1] < this.ids[this.order?.[i] ?? i]);
    }
    for (const id of this.metadata.get("") ?? []) {
      requireSupported(this.read(id).every((entity) => Object.hasOwn(compoundTypes, entity.name)));
    }
  }
  read(id: number): Entity[] {
    let lo = 0;
    let hi = this.ids.length;
    while (lo < hi) {
      const mid = (lo + hi) >>> 1;
      const candidate = this.order?.[mid] ?? mid;
      if (this.ids[candidate] < id) lo = mid + 1;
      else hi = mid;
    }
    const index = this.order?.[lo] ?? lo;
    requireSupported(lo < this.ids.length && this.ids[index] === id);
    const entities = new Arguments(
      this.decoder.decode(this.bytes.subarray(this.starts[index], this.ends[index])),
    ).read();
    requireSupported(entities.length > 0);
    for (const entity of entities) {
      const arity = Object.hasOwn(supported, entity.name) ? supported[entity.name] : compoundTypes[entity.name];
      requireSupported(arity !== undefined && entity.args.length === arity);
    }
    return entities;
  }
  get(id: number, name: string): Value[] {
    const entities = this.read(id);
    requireSupported(entities.length === 1 && entities[0].name === name);
    return entities[0].args;
  }
  entries(name: string): number[] {
    return this.metadata.get(name) ?? [];
  }
}

export function readFacetedStep(bytes: Uint8Array<ArrayBuffer>): StepMesh[] | null {
  try {
    return new FacetedAssembly(new Records(bytes)).meshes();
  } catch (error) {
    if (error instanceof Unsupported) return null;
    throw error;
  }
}

class FacetedAssembly {
  private readonly points = new Map<number, Vector3>();
  private readonly placements = new Map<number, Matrix4>();
  private readonly colors = new Map<number, [number, number, number]>();
  constructor(private readonly records: Records) {}
  private vector(id: number, name: string): Vector3 {
    const args = this.records.get(id, name);
    const values = list(args[1]);
    requireSupported(values.length === 3);
    return new Vector3(...(values.map(number) as [number, number, number]));
  }
  private point(id: number): Vector3 {
    let point = this.points.get(id);
    if (!point) {
      point = this.vector(id, "CARTESIAN_POINT");
      this.points.set(id, point);
    }
    return point;
  }
  private placement(id: number): Matrix4 {
    let matrix = this.placements.get(id);
    if (matrix) return matrix;
    const args = this.records.get(id, "AXIS2_PLACEMENT_3D");
    const z = args[2] === null ? new Vector3(0, 0, 1) : this.vector(reference(args[2]), "DIRECTION");
    const x = args[3] === null ? new Vector3(1, 0, 0) : this.vector(reference(args[3]), "DIRECTION");
    requireSupported(z.lengthSq() > 0 && x.lengthSq() > 0);
    z.normalize();
    x.addScaledVector(z, -x.dot(z));
    requireSupported(x.lengthSq() > 1e-20);
    x.normalize();
    matrix = new Matrix4()
      .makeBasis(x, new Vector3().crossVectors(z, x), z)
      .setPosition(this.point(reference(args[1])));
    this.placements.set(id, matrix);
    return matrix;
  }
  private unit(context: number): number {
    const calls = this.records.read(context);
    const units = calls.find((entity) => entity.name === "GLOBAL_UNIT_ASSIGNED_CONTEXT");
    requireSupported(
      units && calls.some((entity) => entity.name === "GEOMETRIC_REPRESENTATION_CONTEXT" && entity.args[0] === 3),
    );
    let scale: number | null = null;
    for (const entry of list(units.args[0])) {
      const unit = this.records.read(reference(entry));
      if (!unit.some((entity) => entity.name === "LENGTH_UNIT")) continue;
      const si = unit.find((entity) => entity.name === "SI_UNIT");
      requireSupported(si && enumeration(si.args[1]) === "METRE" && scale === null);
      const prefix = si.args[0] === null ? "" : enumeration(si.args[0]);
      const factor: Record<string, number> = {
        "": 1000,
        CENTI: 10,
        DECI: 100,
        KILO: 1000000,
        MICRO: 0.001,
        MILLI: 1,
        NANO: 0.000001,
      };
      requireSupported(Object.hasOwn(factor, prefix));
      scale = factor[prefix];
    }
    requireSupported(scale !== null);
    return scale;
  }
  private color(id: number, path: Set<number>): [number, number, number] {
    requireSupported(!path.has(id));
    path.add(id);
    const entities = this.records.read(id);
    requireSupported(entities.length === 1);
    const { name, args } = entities[0];
    let children: Value[];
    if (name === "COLOUR_RGB") {
      const rgb = args.slice(1).map(number) as [number, number, number];
      requireSupported(rgb.every((c) => c >= 0 && c <= 1));
      path.delete(id);
      // Match OCCT's Quantity_Color output and Three.js's linear working space.
      const color = new Color().setRGB(rgb[0], rgb[1], rgb[2], SRGBColorSpace);
      return [color.r, color.g, color.b];
    } else if (name === "PRESENTATION_STYLE_ASSIGNMENT") children = list(args[0]);
    else if (name === "SURFACE_STYLE_USAGE") {
      requireSupported(enumeration(args[0]) === "BOTH");
      children = [args[1]];
    } else if (name === "SURFACE_SIDE_STYLE" || name === "FILL_AREA_STYLE") children = list(args[1]);
    else if (name === "SURFACE_STYLE_FILL_AREA") children = [args[0]];
    else if (name === "FILL_AREA_STYLE_COLOUR") children = [args[1]];
    else throw new Unsupported();
    requireSupported(children.length > 0 && path.size < 32);
    const colors = children.map((child) => this.color(reference(child), path));
    requireSupported(colors.every((c) => c.every((v, i) => v === colors[0][i])));
    path.delete(id);
    return colors[0];
  }
  meshes(): StepMesh[] {
    const r = this.records;
    for (const id of r.entries("STYLED_ITEM")) {
      const args = r.get(id, "STYLED_ITEM");
      const target = reference(args[2]);
      r.get(target, "FACETED_BREP");
      const styles = list(args[1]).map((style) => this.color(reference(style), new Set()));
      requireSupported(styles.length > 0 && !this.colors.has(target));
      requireSupported(styles.every((c) => c.every((v, i) => v === styles[0][i])));
      this.colors.set(target, styles[0]);
    }
    const representations = new Map<
      number,
      { items: Set<number>; bodies: number[]; children: { id: number; transform: Matrix4 }[] }
    >();
    let context: number | null = null;
    for (const name of ["SHAPE_REPRESENTATION", "FACETED_BREP_SHAPE_REPRESENTATION"]) {
      for (const id of r.entries(name)) {
        const args = r.get(id, name);
        const unitContext = reference(args[2]);
        if (context === null) context = unitContext;
        requireSupported(context === unitContext);
        const bodies: number[] = [];
        const items = list(args[1]).map(reference);
        for (const item of items) {
          const entity = r.read(item);
          requireSupported(entity.length === 1);
          if (entity[0].name === "FACETED_BREP") bodies.push(item);
          else {
            requireSupported(entity[0].name === "AXIS2_PLACEMENT_3D");
            this.placement(item);
          }
        }
        requireSupported(new Set(bodies).size === bodies.length);
        representations.set(id, { items: new Set(items), bodies, children: [] });
      }
    }
    requireSupported(context !== null && representations.size > 0);
    const products = new Map<number, number>();
    for (const id of r.entries("SHAPE_DEFINITION_REPRESENTATION")) {
      const args = r.get(id, "SHAPE_DEFINITION_REPRESENTATION");
      const shape = r.get(reference(args[0]), "PRODUCT_DEFINITION_SHAPE");
      const product = reference(shape[2]);
      r.get(product, "PRODUCT_DEFINITION");
      requireSupported(!products.has(product) && representations.has(reference(args[1])));
      products.set(product, reference(args[1]));
    }
    requireSupported(
      products.size === representations.size && new Set(products.values()).size === representations.size,
    );
    const children = new Set<number>();
    const relationships = new Set<number>();
    const occurrences = new Set<number>();
    for (const id of r.entries("CONTEXT_DEPENDENT_SHAPE_REPRESENTATION")) {
      const args = r.get(id, "CONTEXT_DEPENDENT_SHAPE_REPRESENTATION");
      const relationshipId = reference(args[0]);
      requireSupported(!relationships.has(relationshipId));
      relationships.add(relationshipId);
      const relationship = r.read(relationshipId);
      requireSupported(relationship.length === 3);
      const rel = relationship.find((entity) => entity.name === "REPRESENTATION_RELATIONSHIP");
      const trans = relationship.find((entity) => entity.name === "REPRESENTATION_RELATIONSHIP_WITH_TRANSFORMATION");
      requireSupported(
        rel && trans && relationship.some((entity) => entity.name === "SHAPE_REPRESENTATION_RELATIONSHIP"),
      );
      const childId = reference(rel.args[2]);
      const parentId = reference(rel.args[3]);
      const parent = representations.get(parentId);
      const child = representations.get(childId);
      requireSupported(parent && child);
      const shape = r.get(reference(args[1]), "PRODUCT_DEFINITION_SHAPE");
      const occurrenceId = reference(shape[2]);
      requireSupported(!occurrences.has(occurrenceId));
      occurrences.add(occurrenceId);
      const occurrence = r.get(occurrenceId, "NEXT_ASSEMBLY_USAGE_OCCURRENCE");
      requireSupported(
        products.get(reference(occurrence[3])) === parentId && products.get(reference(occurrence[4])) === childId,
      );
      const transform = r.get(reference(trans.args[0]), "ITEM_DEFINED_TRANSFORMATION");
      const source = reference(transform[2]);
      const target = reference(transform[3]);
      // Only explicit child-origin to parent placements bypass the CAD kernel.
      // Reversed items and nonidentity source frames need OCCT's frame resolution.
      requireSupported(child.items.has(source) && parent.items.has(target));
      requireSupported(this.placement(source).equals(new Matrix4()));
      const matrix = this.placement(target);
      parent.children.push({ id: childId, transform: matrix });
      children.add(childId);
    }
    requireSupported(occurrences.size === r.entries("NEXT_ASSEMBLY_USAGE_OCCURRENCE").length);
    for (const id of r.entries("")) {
      if (r.read(id).some((entity) => entity.name === "REPRESENTATION_RELATIONSHIP"))
        requireSupported(relationships.has(id));
    }
    const result: StepMesh[] = [];
    const used = new Set<number>();
    const visited = new Set<number>();
    const scale = this.unit(context);
    let outputBytes = 0;
    const visit = (id: number, matrix: Matrix4, path: Set<number>) => {
      requireSupported(!path.has(id) && path.size < 128);
      path.add(id);
      visited.add(id);
      const representation = representations.get(id);
      requireSupported(representation);
      for (const body of representation.bodies) {
        const faces = list(r.get(reference(r.get(body, "FACETED_BREP")[1]), "CLOSED_SHELL")[1]);
        requireSupported(faces.length > 0);
        outputBytes += faces.length * 9 * 4 * 2;
        if (outputBytes > 256 * 1024 * 1024) throw new Error("STEP preview geometry exceeds 256 MiB");
        const positions = new Float32Array(faces.length * 9);
        for (let i = 0; i < faces.length; i++) {
          const triangle = this.triangle(reference(faces[i]));
          for (let j = 0; j < 3; j++)
            triangle[j]
              .clone()
              .applyMatrix4(matrix)
              .multiplyScalar(scale)
              .toArray(positions, i * 9 + j * 3);
        }
        result.push(createStepMesh(positions, null, null, this.colors.get(body) ?? null));
        used.add(body);
      }
      for (const child of representation.children) visit(child.id, matrix.clone().multiply(child.transform), path);
      path.delete(id);
    };
    for (const id of representations.keys()) if (!children.has(id)) visit(id, new Matrix4(), new Set());
    requireSupported(
      result.length > 0 && used.size === r.entries("FACETED_BREP").length && visited.size === representations.size,
    );
    return result;
  }
  private triangle(id: number): Vector3[] {
    const entities = this.records.read(id);
    requireSupported(entities.length === 1);
    const { name, args } = entities[0];
    requireSupported(name === "FACE" || name === "FACE_SURFACE");
    const bounds = list(args[1]);
    requireSupported(bounds.length === 1);
    const bound = this.records.get(reference(bounds[0]), "FACE_OUTER_BOUND");
    const points = list(this.records.get(reference(bound[1]), "POLY_LOOP")[1]);
    requireSupported(points.length === 3);
    const triangle = points.map((value) => this.point(reference(value)));
    requireSupported(flag(bound[2]));
    const cross = new Vector3()
      .subVectors(triangle[1], triangle[0])
      .cross(new Vector3().subVectors(triangle[2], triangle[0]));
    requireSupported(cross.lengthSq() > 0);
    if (name === "FACE_SURFACE") {
      const plane = this.records.get(reference(args[2]), "PLANE");
      const placement = this.records.get(reference(plane[1]), "AXIS2_PLACEMENT_3D");
      const normal = placement[2] === null ? new Vector3(0, 0, 1) : this.vector(reference(placement[2]), "DIRECTION");
      requireSupported(normal.lengthSq() > 0);
      normal.normalize().multiplyScalar(flag(args[3]) ? 1 : -1);
      if (placement[3] !== null) {
        const x = this.vector(reference(placement[3]), "DIRECTION");
        requireSupported(x.cross(normal).lengthSq() > 0);
      }
      requireSupported(cross.clone().normalize().dot(normal) > 1 - 1e-5);
      const origin = this.point(reference(placement[1]));
      requireSupported(Math.abs(new Vector3().subVectors(triangle[0], origin).dot(normal)) < 1e-5);
    }
    return triangle;
  }
}
