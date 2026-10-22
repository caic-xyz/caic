// Tests translucent material restoration, shared materials, and geometry ownership.
import { describe, it } from "node:test";
import { BoxGeometry, Group, Mesh, MeshPhongMaterial, MeshStandardMaterial, PerspectiveCamera } from "three";
import { expect } from "@tests/expect";
import { CadTranslucency } from "./cadTranslucency";

describe("CadTranslucency", () => {
  it("preserves geometry, colors, and existing transparency when toggled", () => {
    const geometry = new BoxGeometry();
    const positions = geometry.getAttribute("position").array;
    const indices = geometry.index;
    const opaque = new MeshStandardMaterial({ color: 0x124578 });
    const transparent = new MeshPhongMaterial({ transparent: true, opacity: 0.6, depthWrite: false });
    const group = new Group();
    group.add(new Mesh(geometry, [opaque, transparent]), new Mesh(geometry, opaque));
    const effect = new CadTranslucency(group);
    const camera = new PerspectiveCamera();
    camera.position.set(3, 4, 5);
    camera.lookAt(0, 0, 0);
    const originalKey = opaque.customProgramCacheKey();
    for (let i = 0; i < 3; i++) {
      effect.setEnabled(true);
      effect.update(camera);
      expect(opaque.transparent).toBe(true);
      expect(opaque.depthWrite).toBe(false);
      expect(opaque.customProgramCacheKey()).not.toBe(originalKey);
      effect.setEnabled(false);
      expect(opaque.transparent).toBe(false);
      expect(opaque.depthWrite).toBe(true);
      expect(opaque.customProgramCacheKey()).toBe(originalKey);
      expect(transparent.transparent).toBe(true);
      expect(transparent.depthWrite).toBe(false);
      expect(transparent.opacity).toBe(0.6);
      expect(opaque.color.getHex()).toBe(0x124578);
      expect(geometry.getAttribute("position").array).toBe(positions);
      expect(geometry.index).toBe(indices);
    }
    geometry.dispose();
    opaque.dispose();
    transparent.dispose();
  });
});
