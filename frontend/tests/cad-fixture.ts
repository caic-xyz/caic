// Creates standard and Bambu 3MF archives with colored component instances for tests.
import { readFileSync } from "node:fs";
import { strToU8, zipSync } from "three/addons/libs/fflate.module.js";

export function create3mfFixture(): Uint8Array<ArrayBuffer> {
  return zipSync({
    "[Content_Types].xml": strToU8(`<?xml version="1.0"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="model" ContentType="application/vnd.ms-package.3dmanufacturing-3dmodel+xml"/>
</Types>`),
    "_rels/.rels": strToU8(`<?xml version="1.0"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Target="/3D/3dmodel.model" Id="rel0" Type="http://schemas.microsoft.com/3dmanufacturing/2013/01/3dmodel"/>
</Relationships>`),
    "3D/3dmodel.model": readFileSync(new URL("./fixtures/colored-components.model", import.meta.url)),
  });
}

export function createBambu3mfFixture(): Uint8Array<ArrayBuffer> {
  return zipSync({
    "_rels/.rels": strToU8(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
      <Relationship Target="/3D/3dmodel.model" Id="rel0" Type="http://schemas.microsoft.com/3dmanufacturing/2013/01/3dmodel"/>
    </Relationships>`),
    "3D/3dmodel.model": readFileSync(new URL("./fixtures/bambu-components.model", import.meta.url)),
    "3D/Objects/parts.model": readFileSync(new URL("./fixtures/bambu-parts.model", import.meta.url)),
    "Metadata/model_settings.config": readFileSync(new URL("./fixtures/bambu-model.config", import.meta.url)),
    "Metadata/project_settings.config": readFileSync(new URL("./fixtures/bambu-project.config", import.meta.url)),
  });
}
