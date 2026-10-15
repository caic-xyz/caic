// Benchmarks STEP archive decompression with a 16 MiB model containing 20% random bytes.
import { gzipSync, zipSync } from "three/addons/libs/fflate.module.js";
import { bench } from "@tests/bench";
import { decompressStep } from "./stepCompression";

const model = new Uint8Array(16 * 1024 * 1024);
let seed = 123;
for (let i = 0; i < model.length; i += 5) {
  seed ^= seed << 13;
  seed ^= seed >>> 17;
  seed ^= seed << 5;
  model[i] = seed & 255;
}
const archive = zipSync({ "model.stp": model });
bench(
  "16 MiB STEP ZIP: decompress and verify CRC",
  async () => {
    await decompressStep(archive.buffer as ArrayBuffer, "model.stpz");
  },
  { time: 500, warmupTime: 100 },
);

const gzip = gzipSync(model);
bench(
  "16 MiB STEP gzip: verify and decompress into one output",
  async () => {
    await decompressStep(gzip.buffer as ArrayBuffer, "model.step.gz");
  },
  { time: 500, warmupTime: 100 },
);
