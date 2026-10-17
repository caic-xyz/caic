// Pipelines bounded STEP downloads through archive validation and incremental indexing before geometry extraction.
import { streamTaskStepFile } from "./taskFile";
import { StepDecompressor } from "./stepCompression";
import { FacetedStepReader } from "./stepFacets";
import type { CadProgress, StepWorkerTimings } from "./stepProtocol";

// Index synchronously as inflate emits bytes, before asking the network for
// more. Only decoded pages survive the stream; archive validation must finish
// before either direct geometry extraction or the OCCT fallback can succeed.
export async function readStepInput(
  taskId: string,
  path: string,
  signal: AbortSignal,
  progress: CadProgress,
  timings: StepWorkerTimings,
) {
  const reader = new FacetedStepReader();
  const decoder = new StepDecompressor(path, (chunk) => {
    const started = performance.now();
    reader.push(chunk);
    timings.directParseMs += performance.now() - started;
  });
  let completed = 0;
  let total: number | null = null;
  const report = () =>
    progress({
      stage: "download",
      completed,
      total,
      decodedBytes: reader.byteLength,
      indexedRecords: reader.indexedRecords,
    });
  const decode = (chunk: Uint8Array<ArrayBuffer> | null) => {
    const started = performance.now();
    const before = timings.directParseMs;
    if (chunk) decoder.push(chunk);
    else decoder.finish();
    timings.decompressMs += performance.now() - started - (timings.directParseMs - before);
  };
  timings.downloadMs = await streamTaskStepFile(
    taskId,
    path,
    signal,
    (chunk) => decode(chunk),
    (received, expected) => {
      completed = received;
      total = expected;
      report();
    },
  );
  decode(null);
  return reader;
}
