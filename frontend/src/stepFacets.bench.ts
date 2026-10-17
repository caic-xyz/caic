// Benchmarks faceted STEP preview import without loading the CAD kernel.
import { bench } from "@tests/bench";
import { createFacetedStep } from "../tests/step-fixture";
import { FacetedStepReader } from "./stepFacets";

const model = createFacetedStep(256, false);
bench(
  "Faceted STEP: 256 colored parts / 1024 triangles",
  () => {
    const reader = new FacetedStepReader();
    for (let offset = 0; offset < model.length; offset += 65536) reader.push(model.subarray(offset, offset + 65536));
    if (!reader.meshes(undefined, undefined)) throw new Error("Faceted STEP benchmark unexpectedly fell back to OCCT");
  },
  { time: 500, warmupTime: 100 },
);

// The supplied 24 MB assembly is intentionally not checked in. To compare its
// 93 MB decoded model locally, feed FacetedStepReader with it in a fresh Node process
// and measure peak RSS with /usr/bin/time -v. OCCT ReadStepFile on the same bytes
// is the baseline; terminate that baseline if it exceeds the chosen time budget.
