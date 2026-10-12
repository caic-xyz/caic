// CAD 3D viewer support: parses and renders STL and STEP models via Three.js and Open CASCADE.

import * as THREE from "three";
import { STLLoader } from "three/addons/loaders/STLLoader.js";
import { OrbitControls } from "three/addons/controls/OrbitControls.js";
import occtimportjs from "occt-import-js";
import type { OcctInstance } from "occt-import-js";
import occtWasmUrl from "occt-import-js/dist/occt-import-js.wasm?url";

export interface CadModelInfo {
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

/** Computes triangle counts and bounding dimensions for a model group. */
export function computeModelInfo(group: THREE.Group): CadModelInfo {
  let triangleCount = 0;
  let meshCount = 0;
  group.traverse((child) => {
    if (child instanceof THREE.Mesh) {
      meshCount++;
      const geom = child.geometry;
      if (geom.index) {
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

/** Initializes an interactive Three.js CAD scene on the provided canvas. */
export function createCadScene(canvas: HTMLCanvasElement): CadScene {
  const renderer = new THREE.WebGLRenderer({ canvas, antialias: true, alpha: true });
  renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, 2));
  renderer.toneMapping = THREE.ACESFilmicToneMapping;
  renderer.toneMappingExposure = 1.0;

  const scene = new THREE.Scene();
  const camera = new THREE.PerspectiveCamera(45, 1, 0.1, 2000);

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
      if (child instanceof THREE.Mesh) {
        child.geometry.dispose();
        if (Array.isArray(child.material)) {
          child.material.forEach((m) => m.dispose());
        } else {
          child.material.dispose();
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

      const isStl = /\.stl$/i.test(filename);
      if (isStl) {
        const geom = parseStl(buffer);
        currentGroup = createStlGroup(geom);
      } else {
        currentGroup = await parseStep(buffer);
      }

      const box = new THREE.Box3().setFromObject(currentGroup);
      const center = box.getCenter(new THREE.Vector3());
      currentGroup.position.sub(center);
      scene.add(currentGroup);

      box.setFromObject(currentGroup);
      fitCamera(box);

      return computeModelInfo(currentGroup);
    },

    setWireframe(enabled: boolean) {
      if (!currentGroup) return;
      currentGroup.traverse((child) => {
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
