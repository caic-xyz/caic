// Applies camera-depth opacity to existing CAD materials without copying mesh buffers.
import type { Camera, Material, Object3D } from "three";
import { Box3, Mesh, Vector2 } from "three";

export class CadTranslucency {
  private enabled = false;
  private readonly depth = { value: new Vector2() };
  private readonly bounds: Box3;
  private readonly viewBounds = new Box3();
  private readonly minimumSpan: number;
  private readonly materials = new Map<Material, { transparent: boolean; depthWrite: boolean }>();

  constructor(root: Object3D) {
    root.updateWorldMatrix(true, true);
    this.bounds = new Box3().setFromObject(root);
    this.minimumSpan = Math.max(this.bounds.min.distanceTo(this.bounds.max) * 1e-6, 1e-6);
    root.traverse((child) => {
      if (!(child instanceof Mesh)) return;
      const materials: Material[] = Array.isArray(child.material) ? child.material : [child.material];
      for (const material of materials) {
        if (this.materials.has(material)) continue;
        this.materials.set(material, { transparent: material.transparent, depthWrite: material.depthWrite });
        const compile = material.onBeforeCompile;
        const key = material.customProgramCacheKey();
        material.customProgramCacheKey = () => `${key}:cad-depth:${this.enabled}`;
        material.onBeforeCompile = (shader, renderer) => {
          compile.call(material, shader, renderer);
          // Cached program variants reuse the material's most recently compiled
          // uniform set. Keep the shared depth range in every variant, including
          // opaque/wireframe shaders, so re-enabling translucency cannot freeze it.
          shader.uniforms.cadDepthRange = this.depth;
          if (!this.enabled) return;
          shader.vertexShader = `varying float vCadDepth;\n${shader.vertexShader}`.replace(
            "#include <project_vertex>",
            "#include <project_vertex>\nvCadDepth = -mvPosition.z;",
          );
          shader.fragmentShader =
            `varying float vCadDepth;\nuniform vec2 cadDepthRange;\n${shader.fragmentShader}`.replace(
              "#include <opaque_fragment>",
              `float cadDepth = clamp((vCadDepth - cadDepthRange.x) / (cadDepthRange.y - cadDepthRange.x), 0.0, 1.0);
diffuseColor.a *= mix(0.15, 0.8, cadDepth);
#include <opaque_fragment>`,
            );
        };
      }
    });
  }

  setEnabled(enabled: boolean) {
    if (this.enabled === enabled) return;
    this.enabled = enabled;
    for (const [material, original] of this.materials) {
      material.transparent = enabled || original.transparent;
      // Transparent surfaces must allow deeper faces to contribute. Normal
      // object sorting is approximate for intersecting or self-overlapping CAD.
      material.depthWrite = enabled ? false : original.depthWrite;
      material.needsUpdate = true;
    }
  }

  update(camera: Camera) {
    if (!this.enabled) return;
    camera.updateMatrixWorld();
    this.viewBounds.copy(this.bounds).applyMatrix4(camera.matrixWorldInverse);
    const center = -(this.viewBounds.min.z + this.viewBounds.max.z) / 2;
    const span = Math.max(this.viewBounds.max.z - this.viewBounds.min.z, this.minimumSpan);
    this.depth.value.set(center - span / 2, center + span / 2);
  }
}
