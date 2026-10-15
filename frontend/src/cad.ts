// CAD viewer: loads STEP in a worker and renders CAD meshes with printing plates and a camera-aligned XYZ frame.

import * as THREE from "three";
import { STLLoader } from "three/addons/loaders/STLLoader.js";
import { ThreeMFLoader } from "three/addons/loaders/3MFLoader.js";
import { isStlPath, isStepPath } from "./runtimePath";
import { loadStep } from "./stepWorkerClient";
import { readTaskBinaryFile } from "./taskFile";
import type { CadProgress, StepMesh } from "./stepProtocol";
import { applyBambuMetadata } from "./bambu3mf";
import { BambuPaintGeometry } from "./bambuPaint";
import type { CadPlate } from "./bambu3mf";
import { OrbitControls } from "three/addons/controls/OrbitControls.js";
import { ViewHelper } from "three/addons/helpers/ViewHelper.js";

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
  loadModel(model: ParsedCad, signal: AbortSignal, progress: CadProgress): Promise<CadModelInfo>;
  clearModel(): void;
  setWireframe(enabled: boolean): void;
  setAxesVisible(visible: boolean): void;
  resetCamera(): void;
  resize(width: number, height: number): void;
  destroy(): void;
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

// Constructs lightweight Three.js objects around transferred arrays without copying
// their buffers. Normals and spatial bounds were computed in the worker.
export async function createStepGroup(
  meshes: StepMesh[],
  signal: AbortSignal,
  progress: CadProgress,
): Promise<THREE.Group> {
  const group = new THREE.Group();
  let start = performance.now();
  progress({ stage: "meshes", completed: 0, total: meshes.length });
  try {
    for (let index = 0; index < meshes.length; index++) {
      signal.throwIfAborted();
      const source = meshes[index];
      const geometry = new THREE.BufferGeometry();
      geometry.setAttribute("position", new THREE.BufferAttribute(source.positions, 3));
      geometry.setAttribute("normal", new THREE.BufferAttribute(source.normals, 3));
      if (source.indices) geometry.setIndex(new THREE.BufferAttribute(source.indices, 1));
      geometry.boundingBox = new THREE.Box3(new THREE.Vector3(...source.min), new THREE.Vector3(...source.max));
      geometry.boundingSphere = new THREE.Sphere(new THREE.Vector3(...source.sphere.center), source.sphere.radius);
      const color = source.color ? new THREE.Color(...source.color) : new THREE.Color(0x8f_a1_b3);
      group.add(
        new THREE.Mesh(
          geometry,
          new THREE.MeshStandardMaterial({ color, roughness: 0.35, metalness: 0.15, side: THREE.DoubleSide }),
        ),
      );
      if (performance.now() - start >= 4) {
        progress({ stage: "meshes", completed: index + 1, total: meshes.length });
        await new Promise<void>((resolve) => setTimeout(resolve, 0));
        start = performance.now();
      }
    }
    signal.throwIfAborted();
    progress({ stage: "meshes", completed: meshes.length, total: meshes.length });
    return group;
  } catch (error) {
    disposeCadGroup(group);
    throw error;
  }
}

// STEP's one-shot worker owns its large input and parser heap; ordinary mesh
// formats retain their existing parser. Both canvas and metadata previews use it.
export async function loadCadFile(
  taskId: string,
  filename: string,
  signal: AbortSignal,
  progress: CadProgress,
): Promise<ParsedCad> {
  signal.throwIfAborted();
  if (isStepPath(filename)) {
    const meshes = await loadStep(taskId, filename, signal, progress);
    signal.throwIfAborted();
    return { group: await createStepGroup(meshes, signal, progress), plates: [] };
  }
  const buffer = await readTaskBinaryFile(taskId, filename, signal, (completed, total) =>
    progress({ stage: "download", completed, total }),
  );
  signal.throwIfAborted();
  progress({ stage: "meshes", completed: null, total: null });
  return parseCad(buffer, filename);
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

// Parses STL and 3MF mesh formats; STEP files are owned by loadCadFile's worker.
export async function parseCad(buffer: ArrayBuffer | Uint8Array<ArrayBuffer>, filename: string): Promise<ParsedCad> {
  // Only compressed STEP downloads use prefix views; ordinary CAD buffers
  // already own exactly their bytes. Respect subviews from other callers.
  const data =
    buffer instanceof ArrayBuffer
      ? buffer
      : buffer.byteOffset === 0 && buffer.byteLength === buffer.buffer.byteLength
        ? buffer.buffer
        : buffer.slice().buffer;
  if (isStlPath(filename)) return { group: createStlGroup(parseStl(data)), plates: [] };
  if (/\.3mf$/i.test(filename)) return parse3mf(data);
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

// Releases model geometry, materials, and generated label textures.
export function disposeCadGroup(group: THREE.Group) {
  group.removeFromParent();
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

// Initializes an interactive CAD scene with plate text and a corner XYZ frame.
export function createCadScene(canvas: HTMLCanvasElement): CadScene {
  const renderer = new THREE.WebGLRenderer({ canvas, antialias: true, alpha: true });
  renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, 2));
  renderer.toneMapping = THREE.ACESFilmicToneMapping;
  renderer.toneMappingExposure = 1.0;
  renderer.autoClear = false;

  const scene = new THREE.Scene();
  const camera = new THREE.PerspectiveCamera(45, 1, 0.1, 2000);
  camera.up.set(0, 0, 1); // CAD models and print beds use Z as the vertical axis.

  // A separate viewport keeps the world axes independent of model scale and
  // panning. ViewHelper applies the inverse camera rotation, including Z-up.
  const axes = new ViewHelper(camera, canvas);
  axes.setLabels("X", "Y", "Z");
  axes.location.top = 8;
  axes.location.right = 8;

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
    renderer.clear();
    renderer.render(scene, camera);
    if (axes.visible) axes.render(renderer);
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

  function clearModel() {
    if (currentGroup) {
      disposeCadGroup(currentGroup);
      currentGroup = null;
    }
  }

  return {
    async loadModel(model: ParsedCad, signal: AbortSignal, progress: CadProgress): Promise<CadModelInfo> {
      const candidate = new THREE.Group();
      candidate.add(model.group);
      let ownsCandidate = true;
      function releaseCandidate() {
        if (ownsCandidate) {
          disposeCadGroup(candidate);
          ownsCandidate = false;
        }
      }
      try {
        signal.throwIfAborted();
        const info = { ...computeModelInfo(model.group), plates: model.plates };
        prepareModelShading(model.group);
        const plates = createPlateGroup(model.plates);
        candidate.add(plates);
        for (const plate of model.plates)
          plates.add(createPlateLabel(plate, Math.min(8, renderer.capabilities.getMaxAnisotropy())));
        const box = new THREE.Box3().setFromObject(candidate);
        const center = box.getCenter(new THREE.Vector3());
        candidate.position.sub(center);
        box.translate(center.negate());
        progress({ stage: "render", completed: null, total: null });
        fitCamera(box);
        // Compile shaders while the loading UI can still paint. The candidate
        // remains detached until this request is still current and ready.
        signal.addEventListener("abort", releaseCandidate, { once: true });
        try {
          await renderer.compileAsync(candidate, camera, scene);
        } finally {
          signal.removeEventListener("abort", releaseCandidate);
        }
        signal.throwIfAborted();
        if (isDestroyed) throw new DOMException("Viewer closed", "AbortError");
        clearModel();
        currentGroup = candidate;
        ownsCandidate = false;
        scene.add(candidate);
        return info;
      } catch (error) {
        releaseCandidate();
        throw error;
      }
    },

    clearModel,

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

    setAxesVisible(visible: boolean) {
      axes.visible = visible;
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
      axes.dispose();
      clearModel();
      renderer.dispose();
    },
  };
}
