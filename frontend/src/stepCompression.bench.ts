// Benchmarks STEP archive decompression with a 16 MiB model containing 20% random bytes.
import { gzipSync, zipSync } from "three/addons/libs/fflate.module.js";
import { bench } from "@tests/bench";
import { StepDecompressor } from "./stepCompression";

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
  () => {
    stream(archive, "model.stpz");
  },
  { time: 500, warmupTime: 100 },
);

const gzip = gzipSync(model);
bench(
  "16 MiB STEP gzip: stream and verify each member",
  () => {
    stream(gzip, "model.step.gz");
  },
  { time: 500, warmupTime: 100 },
);

function stream(bytes: Uint8Array<ArrayBuffer>, filename: string): void {
  let size = 0;
  const decoder = new StepDecompressor(filename, (chunk) => {
    size += chunk.length;
  });
  for (let offset = 0; offset < bytes.length; offset += 65536) decoder.push(bytes.subarray(offset, offset + 65536));
  decoder.finish();
  if (size !== model.length) throw new Error("Incomplete decompression");
}
