// CAD viewer: renders models with Bambu filament colors, readable dark shading, and labeled printing plates.

import * as THREE from "three";
import { STLLoader } from "three/addons/loaders/STLLoader.js";
import { ThreeMFLoader } from "three/addons/loaders/3MFLoader.js";
import { isStlPath, isStepPath } from "./runtimePath";
import { applyBambuMetadata } from "./bambu3mf";
import { BambuPaintGeometry } from "./bambuPaint";
import type { CadPlate } from "./bambu3mf";
import { OrbitControls } from "three/addons/controls/OrbitControls.js";
import occtimportjs from "occt-import-js";
import type { OcctInstance } from "occt-import-js";
import occtWasmUrl from "occt-import-js/dist/occt-import-js.wasm?url";

export interface ParsedCad {
  group: THREE.Group;
  plates: CadPlate[];
}

export interface CadModelInfo {
  plates: CadPlate[];
  triangleCount: number;
  meshCount: number;
  dimensions: {
    x: number;
    y: number;
    z: number;
  };
}

export interface CadScene {
  loadModel(buffer: ArrayBuffer, filename: string): Promise<CadModelInfo>;
  setWireframe(enabled: boolean): void;
  resetCamera(): void;
  resize(width: number, height: number): void;
  destroy(): void;
}

let occtPromise: Promise<unknown> | null = null;

async function getOcct(): Promise<OcctInstance> {
  if (!occtPromise) {
    const isBrowser = typeof window !== "undefined" && (typeof process === "undefined" || !process.versions?.node);
    occtPromise = occtimportjs({
      locateFile: (name: string, prefix?: string) => {
        if (isBrowser && name.endsWith(".wasm")) {
          return occtWasmUrl;
        }
        return (prefix ?? "") + name;
      },
    });
  }
  return occtPromise as Promise<OcctInstance>;
}

/** Returns true if WebGL canvas rendering is available in the current environment. */
export function isWebGLSupported(): boolean {
  try {
    const canvas = document.createElement("canvas");
    return Boolean(window.WebGLRenderingContext && (canvas.getContext("webgl2") || canvas.getContext("webgl")));
  } catch {
    return false;
  }
}

/** Parses an STL binary or ASCII buffer into a Three.js BufferGeometry. */
export function parseStl(buffer: ArrayBuffer): THREE.BufferGeometry {
  const loader = new STLLoader();
  const geometry = loader.parse(buffer);
  if (!geometry.attributes.normal) {
    geometry.computeVertexNormals();
  }
  return geometry;
}

/** Wraps a single STL geometry in a standard CAD mesh and group. */
export function createStlGroup(geometry: THREE.BufferGeometry): THREE.Group {
  const material = new THREE.MeshStandardMaterial({
    color: 0x8f_a1_b3,
    roughness: 0.35,
    metalness: 0.15,
    side: THREE.DoubleSide,
  });
  const mesh = new THREE.Mesh(geometry, material);
  const group = new THREE.Group();
  group.add(mesh);
  return group;
}

/** Parses a STEP or STP buffer into a Three.js Group with meshes and colors. */
export async function parseStep(buffer: ArrayBuffer): Promise<THREE.Group> {
  const occt = await getOcct();
  const fileBytes = new Uint8Array(buffer);
  const result = occt.ReadStepFile(fileBytes, null);
  if (!result || !result.success) {
    throw new Error("Failed to parse STEP file: invalid geometry or syntax");
  }

  const group = new THREE.Group();
  for (const meshData of result.meshes) {
    const geometry = new THREE.BufferGeometry();
    geometry.setAttribute("position", new THREE.Float32BufferAttribute(meshData.attributes.position.array, 3));
    if (meshData.attributes.normal) {
      geometry.setAttribute("normal", new THREE.Float32BufferAttribute(meshData.attributes.normal.array, 3));
    } else {
      geometry.computeVertexNormals();
    }
    if (meshData.index && meshData.index.array) {
      geometry.setIndex(new THREE.Uint32BufferAttribute(meshData.index.array, 1));
    }
    const color = meshData.color
      ? new THREE.Color(meshData.color[0], meshData.color[1], meshData.color[2])
      : new THREE.Color(0x8f_a1_b3);
    const material = new THREE.MeshStandardMaterial({
      color,
      roughness: 0.35,
      metalness: 0.15,
      side: THREE.DoubleSide,
    });
    group.add(new THREE.Mesh(geometry, material));
  }
  return group;
}

// Parses a 3MF archive while preserving build transforms and materials.
export function parse3mf(buffer: ArrayBuffer): ParsedCad {
  const group = new ThreeMFLoader().parse(buffer);
  const plates = applyBambuMetadata(group, buffer);
  if (computeModelInfo(group).triangleCount === 0) {
    throw new Error("Failed to parse 3MF file: no triangles in build");
  }
  return { group, plates };
}

// Selects the same parser for the canvas and metadata-only viewer.
export async function parseCad(buffer: ArrayBuffer, filename: string): Promise<ParsedCad> {
  if (isStlPath(filename)) return { group: createStlGroup(parseStl(buffer)), plates: [] };
  if (isStepPath(filename)) return { group: await parseStep(buffer), plates: [] };
  if (/\.3mf$/i.test(filename)) return parse3mf(buffer);
  throw new Error("Unsupported CAD file format");
}

/** Computes triangle counts and bounding dimensions for a model group. */
export function computeModelInfo(group: THREE.Group): Omit<CadModelInfo, "plates"> {
  let triangleCount = 0;
  let meshCount = 0;
  group.traverse((child) => {
    if (child instanceof THREE.Mesh) {
      meshCount++;
      const geom = child.geometry;
      if (geom instanceof BambuPaintGeometry) {
        triangleCount += geom.sourceTriangleCount;
      } else if (geom.index) {
        triangleCount += geom.index.count / 3;
      } else if (geom.attributes.position) {
        triangleCount += geom.attributes.position.count / 3;
      }
    }
  });

  const box = new THREE.Box3().setFromObject(group);
  const size = box.getSize(new THREE.Vector3());

  return {
    triangleCount: Math.round(triangleCount),
    meshCount,
    dimensions: {
      x: Number(size.x.toFixed(2)),
      y: Number(size.y.toFixed(2)),
      z: Number(size.z.toFixed(2)),
    },
  };
}

// Clips a 10 mm grid to the printable polygon, including concave outlines.
function createPlateGrid(outline: CadPlate["outline"]): THREE.BufferGeometry {
  const bounds = new THREE.Box2();
  outline.forEach((p) => bounds.expandByPoint(new THREE.Vector2(p.x, p.y)));
  const size = bounds.getSize(new THREE.Vector2());
  // Bound intersection work for dimensions supplied by untrusted project files.
  if ((Math.ceil(size.x / 10) + Math.ceil(size.y / 10)) * outline.length > 1_000_000) {
    throw new Error("Printing plate is too large for a 10 mm grid");
  }
  const points: THREE.Vector3[] = [];
  for (const axis of ["x", "y"] as const) {
    const other = axis === "x" ? "y" : "x";
    for (let offset = 10; offset < size[axis]; offset += 10) {
      const coordinate = bounds.min[axis] + offset;
      const intersections: number[] = [];
      for (let i = 0; i < outline.length; i++) {
        const a = outline[i];
        const b = outline[(i + 1) % outline.length];
        // Half-open crossings count vertices once and exclude parallel edges.
        if ((a[axis] <= coordinate && b[axis] > coordinate) || (b[axis] <= coordinate && a[axis] > coordinate)) {
          intersections.push(a[other] + ((coordinate - a[axis]) / (b[axis] - a[axis])) * (b[other] - a[other]));
        }
      }
      intersections.sort((a, b) => a - b);
      for (let i = 0; i + 1 < intersections.length; i += 2) {
        for (const value of [intersections[i], intersections[i + 1]]) {
          points.push(new THREE.Vector3(axis === "x" ? coordinate : value, axis === "y" ? coordinate : value, -0.05));
        }
      }
    }
  }
  return new THREE.BufferGeometry().setFromPoints(points);
}

// Builds translucent plate surfaces, outlines, and grids independently of model metrics.
export function createPlateGroup(plates: CadPlate[]): THREE.Group {
  const group = new THREE.Group();
  for (const plate of plates) {
    const grid = new THREE.LineSegments(
      createPlateGrid(plate.outline),
      new THREE.LineBasicMaterial({
        color: 0x263442,
        transparent: true,
        opacity: 0.35,
        depthWrite: false,
      }),
    );
    const shape = new THREE.Shape(plate.outline.map((p) => new THREE.Vector2(p.x, p.y)));
    const surface = new THREE.Mesh(
      new THREE.ShapeGeometry(shape),
      new THREE.MeshStandardMaterial({
        color: 0x7b8794,
        roughness: 1,
        metalness: 0,
        side: THREE.DoubleSide,
        transparent: true,
        opacity: 0.25,
        depthWrite: false,
      }),
    );
    surface.position.z = -0.1;
    surface.name = `Plate ${plate.id}: ${plate.name}`;
    const line = new THREE.LineLoop(
      new THREE.BufferGeometry().setFromPoints(plate.outline.map((p) => new THREE.Vector3(p.x, p.y, -0.05))),
      new THREE.LineBasicMaterial({ color: 0x263442 }),
    );
    group.add(surface, line, grid);
  }
  return group;
}

// Pure black absorbs all diffuse light. Give dark materials a small display-only
// reflectance floor and visible highlights without changing parsed filament colors.
export function prepareModelShading(group: THREE.Group): void {
  const materials = new Map<THREE.Material, THREE.Material>();
  function prepare(source: THREE.Material): THREE.Material {
    const cached = materials.get(source);
    if (cached) return cached;
    if (
      !(source instanceof THREE.MeshPhongMaterial || source instanceof THREE.MeshStandardMaterial) ||
      Math.max(source.color.r, source.color.g, source.color.b) >= 0.02
    )
      return source;
    const material = source.clone();
    material.color.addScalar(0.015);
    if (material instanceof THREE.MeshPhongMaterial) {
      material.specular.setHex(0x444444);
      material.shininess = 30;
    }
    materials.set(source, material);
    return material;
  }
  group.traverse((child) => {
    if (child instanceof THREE.Mesh) {
      child.material = Array.isArray(child.material) ? child.material.map(prepare) : prepare(child.material);
    }
  });
}

// Renders transparent text in the plate's XY plane, above its top edge.
function createPlateLabel(
  plate: CadPlate,
  anisotropy: number,
): THREE.Mesh<THREE.PlaneGeometry, THREE.MeshBasicMaterial> {
  const canvas = document.createElement("canvas");
  canvas.width = 1024;
  const context = canvas.getContext("2d");
  if (!context) throw new Error("Cannot render printing plate text");
  context.font = "48px sans-serif";
  const lines: string[] = [];
  let line = "";
  for (const word of `Plate ${plate.id}${plate.name ? `: ${plate.name}` : ""}`.split(/\s+/)) {
    const candidate = line ? `${line} ${word}` : word;
    if (line && context.measureText(candidate).width > 960) {
      lines.push(line);
      line = word;
    } else {
      line = candidate;
    }
  }
  lines.push(line);
  const textWidth = lines.reduce((max, text) => Math.max(max, Math.min(960, context.measureText(text).width)), 0);
  canvas.width = Math.ceil(textWidth + 32);
  // Cap the texture size even when a project supplies a very long plate name.
  canvas.height = Math.min(1024, lines.length * 60 + 32);
  const lineHeight = (canvas.height - 32) / lines.length;
  context.font = `${Math.min(48, lineHeight / 1.25)}px sans-serif`;
  context.fillStyle = new THREE.Color(0x263442).getStyle();
  context.textAlign = "center";
  context.textBaseline = "middle";
  lines.forEach((text, i) => context.fillText(text, canvas.width / 2, 16 + (i + 0.5) * lineHeight, canvas.width - 32));
  const texture = new THREE.CanvasTexture(canvas);
  texture.colorSpace = THREE.SRGBColorSpace;
  texture.anisotropy = anisotropy;
  const bounds = new THREE.Box2();
  plate.outline.forEach((p) => bounds.expandByPoint(new THREE.Vector2(p.x, p.y)));
  const size = bounds.getSize(new THREE.Vector2());
  const aspect = canvas.height / canvas.width;
  const width = Math.min(size.x * 0.9, (size.y * 0.16) / aspect);
  const height = width * aspect;
  const label = new THREE.Mesh(
    new THREE.PlaneGeometry(width, height),
    new THREE.MeshBasicMaterial({
      map: texture,
      transparent: true,
      depthWrite: false,
      side: THREE.DoubleSide,
      toneMapped: false,
    }),
  );
  label.name = `Plate ${plate.id}: ${plate.name}`;
  label.position.set((bounds.min.x + bounds.max.x) / 2, bounds.max.y + size.y * 0.025 + height / 2, 0);
  return label;
}

/** Initializes an interactive CAD scene with flat plate text. */
export function createCadScene(canvas: HTMLCanvasElement): CadScene {
  const renderer = new THREE.WebGLRenderer({ canvas, antialias: true, alpha: true });
  renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, 2));
  renderer.toneMapping = THREE.ACESFilmicToneMapping;
  renderer.toneMappingExposure = 1.0;

  const scene = new THREE.Scene();
  const camera = new THREE.PerspectiveCamera(45, 1, 0.1, 2000);
  camera.up.set(0, 0, 1); // CAD models and print beds use Z as the vertical axis.

  const controls = new OrbitControls(camera, canvas);
  controls.enableDamping = true;
  controls.dampingFactor = 0.05;
  controls.screenSpacePanning = true;

  const ambientLight = new THREE.AmbientLight(0xffffff, 0.7);
  scene.add(ambientLight);
  const hemiLight = new THREE.HemisphereLight(0xffffff, 0x333333, 0.6);
  scene.add(hemiLight);
  const dirLight1 = new THREE.DirectionalLight(0xffffff, 1.2);
  dirLight1.position.set(1, 1.5, 1).normalize();
  scene.add(dirLight1);
  const dirLight2 = new THREE.DirectionalLight(0xffffff, 0.5);
  dirLight2.position.set(-1, -1, -1).normalize();
  scene.add(dirLight2);

  let currentGroup: THREE.Group | null = null;
  const initialCameraPos = new THREE.Vector3();
  let animId: number | null = null;
  let isDestroyed = false;

  function animate() {
    if (isDestroyed) return;
    animId = requestAnimationFrame(animate);
    controls.update();
    renderer.render(scene, camera);
  }
  animate();

  function fitCamera(box: THREE.Box3) {
    const size = box.getSize(new THREE.Vector3());
    const maxDim = Math.max(size.x, size.y, size.z) || 1;
    const fov = (camera.fov * Math.PI) / 180;
    const dist = (maxDim / 2 / Math.tan(fov / 2)) * 1.6;
    initialCameraPos.set(dist, dist * 0.8, dist * 1.2);
    camera.position.copy(initialCameraPos);
    camera.near = Math.max(maxDim * 0.01, 0.001);
    camera.far = maxDim * 100;
    camera.updateProjectionMatrix();
    controls.target.set(0, 0, 0);
    controls.maxDistance = maxDim * 20;
    controls.minDistance = maxDim * 0.05;
    controls.update();
  }

  function disposeGroup(group: THREE.Group) {
    scene.remove(group);
    group.traverse((child) => {
      if (child instanceof THREE.Mesh || child instanceof THREE.Line) {
        child.geometry.dispose();
        const materials = Array.isArray(child.material) ? child.material : [child.material];
        for (const material of materials) {
          if (material instanceof THREE.MeshBasicMaterial && material.map instanceof THREE.CanvasTexture) {
            material.map.dispose();
          }
          material.dispose();
        }
      }
    });
  }

  return {
    async loadModel(buffer: ArrayBuffer, filename: string): Promise<CadModelInfo> {
      if (currentGroup) {
        disposeGroup(currentGroup);
        currentGroup = null;
      }

      const model = await parseCad(buffer, filename);
      const info = { ...computeModelInfo(model.group), plates: model.plates };
      prepareModelShading(model.group);
      const plates = createPlateGroup(model.plates);
      for (const plate of model.plates) {
        plates.add(createPlateLabel(plate, Math.min(8, renderer.capabilities.getMaxAnisotropy())));
      }
      currentGroup = new THREE.Group();
      currentGroup.add(model.group, plates);

      const box = new THREE.Box3().setFromObject(currentGroup);
      const center = box.getCenter(new THREE.Vector3());
      currentGroup.position.sub(center);
      scene.add(currentGroup);

      box.setFromObject(currentGroup);
      fitCamera(box);

      return info;
    },

    setWireframe(enabled: boolean) {
      if (!currentGroup) return;
      currentGroup.children[0].traverse((child) => {
        if (child instanceof THREE.Mesh) {
          if (Array.isArray(child.material)) {
            child.material.forEach((m) => {
              m.wireframe = enabled;
            });
          } else {
            child.material.wireframe = enabled;
          }
        }
      });
    },

    resetCamera() {
      camera.position.copy(initialCameraPos);
      controls.target.set(0, 0, 0);
      controls.update();
    },

    resize(width: number, height: number) {
      if (width <= 0 || height <= 0) return;
      camera.aspect = width / height;
      camera.updateProjectionMatrix();
      renderer.setSize(width, height, false);
    },

    destroy() {
      isDestroyed = true;
      if (animId !== null) {
        cancelAnimationFrame(animId);
        animId = null;
      }
      controls.dispose();
      if (currentGroup) {
        disposeGroup(currentGroup);
        currentGroup = null;
      }
      renderer.dispose();
    },
  };
}
