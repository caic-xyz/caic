// Incrementally indexes paged STEP bytes and resolves complete planar faceted assemblies; unsupported models use OCCT.
import { Color, Matrix4, ShapeUtils, SRGBColorSpace, Vector2, Vector3 } from "three";
import { createStepMesh } from "./stepMesh";
import { StepBytes } from "./stepBytes";
import type { CadProgress, StepImportTimings, StepMesh } from "./stepProtocol";

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

// Incremental lexical states retain only the boundary state, never a growing
// record string. Quotes, doubled quotes and comments can straddle any chunk.
class Scanner {
  private offset = 0;
  private start = 0;
  private state: "normal" | "quote" | "after-quote" | "slash" | "comment" | "star" = "normal";
  private section = 0;
  constructor(
    private readonly bytes: StepBytes,
    private readonly visit: (id: number, start: number, end: number) => void,
  ) {}

  push(chunk: Uint8Array) {
    for (let i = 0; i < chunk.length; i++, this.offset++) {
      const c = chunk[i];
      if (this.state === "comment" || this.state === "star") {
        this.state = this.state === "star" && c === 47 ? "normal" : c === 42 ? "star" : "comment";
        continue;
      }
      if (this.state === "quote") {
        if (c === 39) this.state = "after-quote";
        continue;
      }
      if (this.state === "after-quote") {
        if (c === 39) {
          this.state = "quote";
          continue;
        }
        this.state = "normal";
      }
      if (this.state === "slash") {
        this.state = "normal";
        if (c === 42) {
          this.state = "comment";
          continue;
        }
      }
      if (c === 47) this.state = "slash";
      else if (c === 39) this.state = "quote";
      else if (c === 59) {
        this.record(this.offset);
        this.start = this.offset + 1;
      }
    }
  }
  private record(end: number) {
    const bytes = this.bytes;
    let p = this.start;
    while (p < end) {
      const byte = bytes.byte(p);
      if (byte !== undefined && byte <= 32) p++;
      else if (bytes.byte(p) === 47 && bytes.byte(p + 1) === 42) {
        p += 2;
        while (p < end && !(bytes.byte(p) === 42 && bytes.byte(p + 1) === 47)) p++;
        p += 2;
      } else break;
    }
    if (bytes.byte(p) === 35) {
      requireSupported(this.section === 4);
      let id = 0;
      p++;
      const digits = p;
      while (true) {
        const digit = bytes.byte(p);
        if (digit === undefined || digit < 48 || digit > 57) break;
        id = id * 10 + digit - 48;
        p++;
      }
      requireSupported(p > digits && id > 0 && id <= 0xffffffff);
      while (true) {
        const byte = bytes.byte(p);
        if (byte === undefined || byte > 32) break;
        p++;
      }
      requireSupported(bytes.byte(p++) === 61);
      this.visit(id, p, end);
    } else {
      // Headers are small metadata; avoid decoding an unbounded non-entity
      // statement into a second whole-file string before opting out.
      requireSupported(end - p <= 1024 * 1024);
      const statement = bytes.text(p, end).trim();
      if (this.section === 0 && statement === "ISO-10303-21") this.section = 1;
      else if (this.section === 1 && statement === "HEADER") this.section = 2;
      else if (this.section === 2 && statement === "ENDSEC") this.section = 3;
      else if (this.section === 3 && statement === "DATA") this.section = 4;
      else if (this.section === 4 && statement === "ENDSEC") this.section = 5;
      else if (this.section === 5 && statement === "END-ISO-10303-21") this.section = 6;
      else {
        requireSupported(this.section === 2);
        const header = new Arguments(statement).read();
        requireSupported(
          header.length === 1 &&
            header[0] !== undefined &&
            ["FILE_DESCRIPTION", "FILE_NAME", "FILE_SCHEMA"].includes(header[0].name),
        );
      }
    }
  }
  finish() {
    requireSupported((this.state === "normal" || this.state === "after-quote") && this.section === 6);
    for (let i = this.start; i < this.offset; i++) {
      const byte = this.bytes.byte(i);
      requireSupported(byte !== undefined && byte <= 32);
    }
  }
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
  // Counting shell references before extraction establishes truthful progress
  // without allocating a decoded Value object for each face in the assembly.
  shellFaceCount(): number {
    this.expect("CLOSED_SHELL");
    this.expect("(");
    requireSupported(this.next().startsWith("'"));
    this.expect(",");
    this.expect("(");
    let count = 0;
    let token = this.next();
    if (token !== ")")
      while (true) {
        requireSupported(/^#[0-9]+$/.test(token));
        count++;
        token = this.next();
        if (token === ")") break;
        requireSupported(token === ",");
        token = this.next();
      }
    this.expect(")");
    requireSupported(this.text.slice(this.offset).trim() === "");
    return count;
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

// Fixed index pages avoid a counting pass and growing whole-index copies.
const indexPageSize = 65536;
class Records {
  private readonly pages: Uint32Array[] = [];
  private count = 0;
  private sorted = true;
  private order: Uint32Array | null = null;
  readonly metadata = new Map<string, number[]>();
  private readonly scanner: Scanner;
  constructor(private readonly bytes: StepBytes) {
    this.scanner = new Scanner(bytes, (id, start, end) => {
      const head = bytes.text(start, Math.min(start + 128, end));
      const match = /^\s*([A-Z_][A-Z_0-9]*|\()/.exec(head);
      requireSupported(match && match[1] !== undefined);
      const name = match[1] === "(" ? "" : match[1];
      requireSupported(name === "" || Object.hasOwn(supported, name));
      if (!geometryTypes.has(name)) {
        const entries = this.metadata.get(name);
        if (entries) entries.push(id);
        else this.metadata.set(name, [id]);
      }
      if (this.count && id <= this.field(this.count - 1, 0)) this.sorted = false;
      if (this.count % indexPageSize === 0) this.pages.push(new Uint32Array(indexPageSize * 3));
      const page = this.pages[this.pages.length - 1];
      requireSupported(page);
      const offset = (this.count % indexPageSize) * 3;
      page[offset] = id;
      page[offset + 1] = start;
      page[offset + 2] = end;
      this.count++;
    });
  }
  push(chunk: Uint8Array) {
    this.scanner.push(chunk);
  }
  get length(): number {
    return this.count;
  }
  private field(index: number, field: number): number {
    const page = this.pages[Math.floor(index / indexPageSize)];
    requireSupported(page);
    const value = page[(index % indexPageSize) * 3 + field];
    requireSupported(value !== undefined);
    return value;
  }
  finish() {
    this.scanner.finish();
    requireSupported(this.metadata.has("FACETED_BREP"));
    this.order = this.sorted
      ? null
      : Uint32Array.from({ length: this.count }, (_, i) => i).sort((a, b) => this.field(a, 0) - this.field(b, 0));
    for (let i = 1; i < this.count; i++)
      requireSupported(this.field(this.order?.[i - 1] ?? i - 1, 0) < this.field(this.order?.[i] ?? i, 0));
    for (const id of this.metadata.get("") ?? [])
      requireSupported(this.read(id).every((entity) => Object.hasOwn(compoundTypes, entity.name)));
  }
  private text(id: number): string {
    let lo = 0;
    let hi = this.count;
    while (lo < hi) {
      const mid = (lo + hi) >>> 1;
      const candidate = this.order?.[mid] ?? mid;
      if (this.field(candidate, 0) < id) lo = mid + 1;
      else hi = mid;
    }
    const index = this.order?.[lo] ?? lo;
    requireSupported(lo < this.count && this.field(index, 0) === id);
    return this.bytes.text(this.field(index, 1), this.field(index, 2));
  }
  shellFaceCount(id: number): number {
    return new Arguments(this.text(id)).shellFaceCount();
  }
  read(id: number): Entity[] {
    const entities = new Arguments(this.text(id)).read();
    requireSupported(entities.length > 0);
    for (const entity of entities) {
      const arity = Object.hasOwn(supported, entity.name) ? supported[entity.name] : compoundTypes[entity.name];
      requireSupported(arity !== undefined && entity.args.length === arity);
    }
    return entities;
  }
  get(id: number, name: string): Value[] {
    const entities = this.read(id);
    const entity = entities[0];
    requireSupported(entities.length === 1 && entity?.name === name);
    return entity.args;
  }
  entries(name: string): number[] {
    return this.metadata.get(name) ?? [];
  }
}

export class FacetedStepReader {
  private readonly bytes = new StepBytes();
  private records: Records | null = new Records(this.bytes);
  private state: "reading" | "fallback" | "consumed" = "reading";
  get byteLength(): number {
    return this.bytes.length;
  }
  takeContiguous(): Uint8Array<ArrayBuffer> {
    if (this.state !== "fallback") throw new Error("STEP input is not available for kernel parsing");
    this.state = "consumed";
    return this.bytes.takeContiguous();
  }
  get indexedRecords(): number | null {
    return this.records?.length ?? null;
  }
  push(chunk: Uint8Array) {
    if (this.state !== "reading") throw new Error("STEP input is already finalized");
    this.bytes.append(chunk);
    try {
      this.records?.push(chunk);
    } catch (error) {
      if (!(error instanceof Unsupported)) throw error;
      this.records = null;
    }
  }
  meshes(timings: StepImportTimings | undefined, progress: CadProgress | undefined): StepMesh[] | null {
    if (this.state !== "reading") throw new Error("STEP input is already finalized");
    this.state = "consumed";
    if (!this.records) {
      this.state = "fallback";
      return null;
    }
    try {
      this.records.finish();
      const meshes = new FacetedAssembly(this.records, timings, progress).meshes();
      this.bytes.release();
      return meshes;
    } catch (error) {
      if (!(error instanceof Unsupported)) throw error;
      this.state = "fallback";
      return null;
    } finally {
      this.records = null;
    }
  }
}

class FacetedAssembly {
  private readonly points = new Map<number, Vector3>();
  private readonly placements = new Map<number, Matrix4>();
  private polygonChecks = 0;
  private readonly colors = new Map<number, [number, number, number]>();
  constructor(
    private readonly records: Records,
    private readonly timings: StepImportTimings | undefined,
    private readonly progress: CadProgress | undefined,
  ) {}
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
    requireSupported(
      z.lengthSq() > 0 && x.lengthSq() > 0 && Number.isFinite(z.lengthSq()) && Number.isFinite(x.lengthSq()),
    );
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
      const value = factor[prefix];
      requireSupported(Object.hasOwn(factor, prefix) && value !== undefined);
      scale = value;
    }
    requireSupported(scale !== null);
    return scale;
  }
  private color(id: number, path: Set<number>): [number, number, number] {
    requireSupported(!path.has(id));
    path.add(id);
    const entities = this.records.read(id);
    const entity = entities[0];
    requireSupported(entities.length === 1 && entity);
    const { name, args } = entity;
    let children: (Value | undefined)[];
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
    const first = colors[0];
    requireSupported(first && colors.every((c) => c.every((v, i) => v === first[i])));
    path.delete(id);
    return first;
  }
  meshes(): StepMesh[] {
    const r = this.records;
    for (const id of r.entries("STYLED_ITEM")) {
      const args = r.get(id, "STYLED_ITEM");
      const target = reference(args[2]);
      r.get(target, "FACETED_BREP");
      const styles = list(args[1]).map((style) => this.color(reference(style), new Set()));
      const first = styles[0];
      requireSupported(first && !this.colors.has(target));
      requireSupported(styles.every((c) => c.every((v, i) => v === first[i])));
      this.colors.set(target, first);
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
          const first = entity[0];
          requireSupported(entity.length === 1 && first);
          if (first.name === "FACETED_BREP") bodies.push(item);
          else {
            requireSupported(first.name === "AXIS2_PLACEMENT_3D");
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
      const shape = r.get(reference(args[1]), "PRODUCT_DEFINITION_SHAPE");
      const occurrenceId = reference(shape[2]);
      requireSupported(!occurrences.has(occurrenceId));
      occurrences.add(occurrenceId);
      const occurrence = r.get(occurrenceId, "NEXT_ASSEMBLY_USAGE_OCCURRENCE");
      // Product ownership resolves the relationship independently of its order.
      const parentId = products.get(reference(occurrence[3]));
      const childId = products.get(reference(occurrence[4]));
      requireSupported(parentId !== undefined && childId !== undefined && parentId !== childId);
      requireSupported(
        (reference(rel.args[2]) === childId && reference(rel.args[3]) === parentId) ||
          (reference(rel.args[2]) === parentId && reference(rel.args[3]) === childId),
      );
      const parent = representations.get(parentId);
      const child = representations.get(childId);
      requireSupported(parent && child);
      const transform = r.get(reference(trans.args[0]), "ITEM_DEFINED_TRANSFORMATION");
      const first = reference(transform[2]);
      const second = reference(transform[3]);
      const forward = child.items.has(first) && parent.items.has(second);
      const reverse = child.items.has(second) && parent.items.has(first);
      // OCCT repairs swapped items by ownership. Shared frames that permit
      // both interpretations remain ambiguous and require the complete kernel.
      requireSupported(forward !== reverse);
      const source = this.placement(forward ? first : second);
      const target = this.placement(forward ? second : first);
      // Match ComputeTransformation's SetTransformation(target, source): S^-1*T,
      // not T*S^-1. See OCCT d2abb6d's STEPControl_ActorRead.cxx:1806 and
      // gp_Trsf.cxx:206 (the version pinned by occt-import-js 0.0.23).
      const matrix = source.clone().invert().multiply(target);
      parent.children.push({ id: childId, transform: matrix });
      children.add(childId);
    }
    requireSupported(occurrences.size === r.entries("NEXT_ASSEMBLY_USAGE_OCCURRENCE").length);
    for (const id of r.entries("")) {
      if (r.read(id).some((entity) => entity.name === "REPRESENTATION_RELATIONSHIP"))
        requireSupported(relationships.has(id));
    }
    // Establish the total over assembled occurrences before extracting faces.
    // Reused parts contribute once per visible instance. Bound output before
    // allocating geometry, and retain only small body jobs alongside the index.
    const jobs: { body: number; shell: number; count: number; matrix: Matrix4 }[] = [];
    const bodyInfo = new Map<number, { shell: number; count: number }>();
    let total = 0;
    const used = new Set<number>();
    const visited = new Set<number>();
    const scale = this.unit(context);
    const outputLimit = 256 * 1024 * 1024;
    let outputBytes = 0;
    const visit = (id: number, matrix: Matrix4, path: Set<number>) => {
      requireSupported(!path.has(id) && path.size < 128);
      path.add(id);
      visited.add(id);
      const representation = representations.get(id);
      requireSupported(representation);
      for (const body of representation.bodies) {
        let info = bodyInfo.get(body);
        if (!info) {
          const shell = reference(r.get(body, "FACETED_BREP")[1]);
          const count = r.shellFaceCount(shell);
          requireSupported(count > 0);
          info = { shell, count };
          bodyInfo.set(body, info);
        }
        outputBytes += info.count * 9 * 4 * 2;
        if (outputBytes > outputLimit) throw new Error("STEP preview geometry exceeds 256 MiB");
        jobs.push({ body, ...info, matrix });
        total += info.count;
        used.add(body);
      }
      for (const child of representation.children) visit(child.id, matrix.clone().multiply(child.transform), path);
      path.delete(id);
    };
    for (const id of representations.keys()) if (!children.has(id)) visit(id, new Matrix4(), new Set());
    requireSupported(
      jobs.length > 0 && used.size === r.entries("FACETED_BREP").length && visited.size === representations.size,
    );
    const result: StepMesh[] = [];
    let completed = 0;
    const report = () => this.progress?.({ stage: "parse", parseStep: "geometry", completed, total });
    report();
    for (const { body, shell, count, matrix } of jobs) {
      // Decode only the current body's face references, even for reused parts.
      const faces = list(r.get(shell, "CLOSED_SHELL")[1]);
      let positions = new Float32Array(count * 9);
      let offset = 0;
      for (const face of faces) {
        const triangles = this.triangles(reference(face));
        outputBytes += (triangles.length - 3) * 3 * 4 * 2;
        if (outputBytes > outputLimit) throw new Error("STEP preview geometry exceeds 256 MiB");
        const needed = offset + triangles.length * 3;
        if (needed > positions.length) {
          // Only polygonal bodies grow. Triangular imports retain one exact
          // allocation; spare capacity is bounded by the same output budget.
          const capacity = Math.min(
            Math.max(needed, positions.length * 2),
            needed + Math.floor((outputLimit - outputBytes) / 8),
          );
          const grown = new Float32Array(capacity);
          grown.set(positions);
          positions = grown;
        }
        for (const point of triangles) {
          point.clone().applyMatrix4(matrix).multiplyScalar(scale).toArray(positions, offset);
          offset += 3;
        }
        completed++;
        if (completed % 256 === 0) report();
      }
      result.push(
        createStepMesh(
          offset === positions.length ? positions : positions.slice(0, offset),
          null,
          null,
          this.colors.get(body) ?? null,
          this.timings,
        ),
      );
    }
    report();
    return result;
  }
  private triangles(id: number): Vector3[] {
    const entities = this.records.read(id);
    const entity = entities[0];
    requireSupported(entities.length === 1 && entity);
    const { name, args } = entity;
    requireSupported(name === "FACE" || name === "FACE_SURFACE");
    const bounds = list(args[1]);
    requireSupported(bounds.length === 1);
    const bound = this.records.get(reference(bounds[0]), "FACE_OUTER_BOUND");
    const points = list(this.records.get(reference(bound[1]), "POLY_LOOP")[1]);
    // Quadratic simplicity checks are restricted to small polygons. Large or
    // ambiguous contours use OCCT wholesale rather than risking worker stalls.
    requireSupported(points.length >= 3 && points.length <= 256);
    const triangle = points.map((value) => this.point(reference(value)));
    const [first, second, third] = triangle;
    requireSupported(first && second && third);
    requireSupported(flag(bound[2]));
    const cross = new Vector3().subVectors(second, first).cross(new Vector3().subVectors(third, first));
    if (triangle.length > 3) {
      cross.set(0, 0, 0);
      for (let i = 1; i < triangle.length - 1; i++) {
        const point = triangle[i];
        const next = triangle[i + 1];
        requireSupported(point && next);
        cross.add(new Vector3().subVectors(point, first).cross(new Vector3().subVectors(next, first)));
      }
    }
    requireSupported(cross.lengthSq() > 0 && Number.isFinite(cross.lengthSq()));
    let planeOrigin = first;
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
      planeOrigin = this.point(reference(placement[1]));
      requireSupported(Math.abs(new Vector3().subVectors(first, planeOrigin).dot(normal)) < 1e-5);
      if (triangle.length > 3) cross.copy(normal);
    }
    if (triangle.length === 3) return triangle;
    return this.polygon(triangle, cross.normalize(), planeOrigin);
  }
  private polygon(points: Vector3[], normal: Vector3, planeOrigin: Vector3): Vector3[] {
    const n = points.length;
    this.polygonChecks += (n * (n - 3)) / 2;
    requireSupported(this.polygonChecks <= 2000000);
    const origin = points[0];
    requireSupported(origin);
    const extent = Math.max(...points.map((p) => p.distanceTo(origin)));
    const magnitude = Math.max(...points.flatMap((p) => [Math.abs(p.x), Math.abs(p.y), Math.abs(p.z)]));
    const roundoff = magnitude * Number.EPSILON * 16;
    requireSupported(Number.isFinite(extent) && extent > 0 && roundoff <= extent * 1e-6);
    const tolerance = Math.max(extent * 1e-9, roundoff);
    requireSupported(points.every((p) => Math.abs(new Vector3().subVectors(p, planeOrigin).dot(normal)) <= tolerance));
    const axis =
      Math.abs(normal.x) > Math.abs(normal.y)
        ? Math.abs(normal.x) > Math.abs(normal.z)
          ? 0
          : 2
        : Math.abs(normal.y) > Math.abs(normal.z)
          ? 1
          : 2;
    const contour = points.map((p) => {
      const v = p.clone().sub(origin).divideScalar(extent);
      return axis === 0 ? new Vector2(v.y, v.z) : axis === 1 ? new Vector2(v.x, v.z) : new Vector2(v.x, v.y);
    });
    // Projection coordinates are normalized; tolerances do not depend on STEP length units.
    const epsilon = 1e-10;
    const turn = (a: Vector2, b: Vector2, c: Vector2) => (b.x - a.x) * (c.y - a.y) - (b.y - a.y) * (c.x - a.x);
    for (let i = 0; i < n; i++) {
      const a = contour[i],
        b = contour[(i + 1) % n];
      const next = contour[(i + 2) % n];
      requireSupported(a && b && next);
      requireSupported(Math.abs(turn(a, b, next)) > epsilon);
      for (let j = i + 2; j < n; j++) {
        if (i === 0 && j === n - 1) continue;
        const c = contour[j],
          d = contour[(j + 1) % n];
        requireSupported(c && d);
        if (
          Math.max(a.x, b.x) < Math.min(c.x, d.x) - epsilon ||
          Math.max(c.x, d.x) < Math.min(a.x, b.x) - epsilon ||
          Math.max(a.y, b.y) < Math.min(c.y, d.y) - epsilon ||
          Math.max(c.y, d.y) < Math.min(a.y, b.y) - epsilon
        )
          continue;
        const abC = turn(a, b, c),
          abD = turn(a, b, d),
          cdA = turn(c, d, a),
          cdB = turn(c, d, b);
        requireSupported(
          !(
            Math.min(abC, abD) <= epsilon &&
            Math.max(abC, abD) >= -epsilon &&
            Math.min(cdA, cdB) <= epsilon &&
            Math.max(cdA, cdB) >= -epsilon
          ),
        );
      }
    }
    const area = Math.abs(ShapeUtils.area(contour));
    requireSupported(area > epsilon);
    const faces = ShapeUtils.triangulateShape(contour, []);
    requireSupported(faces.length === n - 2);
    const result: Vector3[] = [];
    let covered = 0;
    for (const [a, b, c] of faces) {
      requireSupported(a !== undefined && b !== undefined && c !== undefined);
      const ca = contour[a],
        cb = contour[b],
        cc = contour[c];
      const pa = points[a],
        pb = points[b],
        pc = points[c];
      requireSupported(ca && cb && cc && pa && pb && pc);
      const triangleArea = Math.abs(turn(ca, cb, cc)) / 2;
      requireSupported(triangleArea > epsilon);
      covered += triangleArea;
      const direction = new Vector3().subVectors(pb, pa).cross(new Vector3().subVectors(pc, pa)).dot(normal);
      requireSupported(Number.isFinite(direction) && direction !== 0);
      result.push(pa, direction > 0 ? pb : pc, direction > 0 ? pc : pb);
    }
    requireSupported(Math.abs(covered - area) <= Math.max(epsilon, area * 1e-9));
    return result;
  }
}
