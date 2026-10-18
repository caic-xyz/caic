// Tests CAD viewer progress stages, cancellation and retry, model metrics, failures, and keyboard navigation.

import { afterEach, beforeEach, describe, it } from "node:test";
import { expect, vi } from "@tests/expect";
import { Route, Router } from "@solidjs/router";
import { cleanup, render, screen, waitFor } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";

import CadViewer from "./CadViewer";
import { create3mfFixture, createBambu3mfFixture } from "../../tests/cad-fixture";
import { stubStepWorkers } from "../../tests/step-worker";
import { createStepMesh } from "../stepMesh";

function renderViewer(path: string) {
  window.history.replaceState(null, "", `/task/@abc/view?path=${encodeURIComponent(path)}`);
  return render(() => (
    <Router>
      <Route path="/*" component={() => <CadViewer taskId="abc" taskPath="/task/@abc+test" path={path} />} />
    </Router>
  ));
}

describe("CadViewer", () => {
  const fetchMock = vi.spyOn(globalThis, "fetch");

  beforeEach(() => {
    fetchMock.mockReset();
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("renders controlled STEP download, decompression, and parsing progress until the result arrives", async () => {
    const workers = stubStepWorkers();
    renderViewer("/models/part.stpz");
    await waitFor(() => expect(workers).toHaveLength(1));
    const worker = workers[0];
    worker.send({ type: "progress", progress: { stage: "download", completed: 25, total: 100 } });
    const download = screen.getByRole("progressbar", { name: "Downloading model" });
    expect(download).toHaveAttribute("value", "25");
    expect(download).toHaveAttribute("max", "100");
    expect(screen.getByText("25%")).toBeInTheDocument();
    worker.send({ type: "progress", progress: { stage: "download", completed: 1048576, total: null } });
    expect(screen.getByRole("progressbar", { name: "Downloading model" })).not.toHaveAttribute("value");
    expect(screen.getByText("1.0 MiB received")).toBeInTheDocument();
    worker.send({ type: "progress", progress: { stage: "decompress", completed: 50, total: 100 } });
    expect(screen.getByRole("progressbar", { name: "Decompressing STEP" })).toHaveAttribute("value", "50");
    worker.send({ type: "progress", progress: { stage: "parse", completed: null, total: null } });
    expect(screen.getByRole("progressbar", { name: "Parsing STEP" })).not.toHaveAttribute("value");
    expect(screen.getByRole("status")).toHaveTextContent("Parsing STEP…");
    expect(screen.getByRole("button", { name: "Cancel loading" })).toBeInTheDocument();
    worker.send({
      type: "result",
      meshes: [createStepMesh(new Float32Array([0, 0, 0, 1, 0, 0, 0, 1, 0]), null, null, null)],
    });
    expect(await screen.findByText("1 triangles")).toBeInTheDocument();
    expect(screen.queryByRole("progressbar")).not.toBeInTheDocument();
    expect(worker.terminate).toHaveBeenCalledTimes(1);
  });

  it("cancels a controlled STEP parse and retries with a new worker", async () => {
    const user = userEvent.setup();
    const workers = stubStepWorkers();
    renderViewer("/models/part.stpz");
    await waitFor(() => expect(workers).toHaveLength(1));
    workers[0].send({ type: "progress", progress: { stage: "parse", completed: null, total: null } });
    await user.click(screen.getByRole("button", { name: "Cancel loading" }));
    expect(screen.getByRole("status")).toHaveTextContent("Model loading cancelled.");
    expect(screen.getByRole("button", { name: "Retry" })).toHaveFocus();
    expect(workers[0].terminate).toHaveBeenCalledTimes(1);
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(workers).toHaveLength(2));
    expect(screen.getByRole("progressbar", { name: "Downloading model" })).toBeInTheDocument();
    workers[1].send({
      type: "result",
      meshes: [createStepMesh(new Float32Array([0, 0, 0, 1, 0, 0, 0, 1, 0]), null, null, null)],
    });
    expect(await screen.findByText("1 triangles")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("loads an STL model, parses its metrics, and displays the header and raw link", async () => {
    const stlContent = `solid sample
facet normal 0 0 1
  outer loop
    vertex 0 0 0
    vertex 5 0 0
    vertex 0 5 0
  endloop
endfacet
endsolid sample
`;
    const encoder = new TextEncoder();
    const stlBytes = encoder.encode(stlContent);

    fetchMock.mockResolvedValue(new Response(stlBytes));
    renderViewer("/home/user/models/sample.stl");

    expect(screen.getByText("/home/user/models/sample.stl")).toBeInTheDocument();
    const rawLink = screen.getByRole("link", { name: "raw file" });
    expect(rawLink).toHaveAttribute("href", "/api/caic/v1/tasks/abc/raw?path=%2Fhome%2Fuser%2Fmodels%2Fsample.stl");
    expect(rawLink).toHaveAttribute("target", "_blank");

    // Once parsed, the triangle badge appears
    expect(await screen.findByText(/1 triangles/)).toBeInTheDocument();
  });

  it("loads 3MF metadata without WebGL", async () => {
    fetchMock.mockResolvedValue(new Response(create3mfFixture()));
    renderViewer("/models/assembly.3mf");
    expect(await screen.findByText("2 triangles")).toHaveAttribute("title", "Dimensions: 30 × 20 × 0");
  });

  it("cancels loading, retries, and ignores a late failure from the cancelled request", async () => {
    const user = userEvent.setup();
    let rejectFirst: (reason: Error) => void = () => {
      throw new Error("Request not started");
    };
    fetchMock.mockImplementationOnce(
      () =>
        new Promise<Response>((_resolve, reject) => {
          rejectFirst = reject;
        }),
    );
    fetchMock.mockResolvedValue(new Response(create3mfFixture()));
    renderViewer("/models/assembly.3mf");
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(screen.getByRole("progressbar", { name: "Downloading model" })).not.toHaveAttribute("value");
    await user.click(screen.getByRole("button", { name: "Cancel loading" }));
    expect(screen.getByRole("status")).toHaveTextContent("Model loading cancelled");
    expect(fetchMock.mock.calls[0][1]?.signal?.aborted).toBe(true);
    expect(screen.getByRole("button", { name: "Retry" })).toHaveFocus();
    await user.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("2 triangles")).toBeInTheDocument();
    rejectFirst(new Error("Late failure"));
    // Flush the rejected fetch's continuation after the replacement has loaded.
    await new Promise<void>((resolve) => setImmediate(resolve));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByText("2 triangles")).toBeInTheDocument();
  });

  it("loads Bambu 3MF model metrics without WebGL", async () => {
    fetchMock.mockResolvedValue(new Response(createBambu3mfFixture()));
    renderViewer("/models/bambu.3mf");
    expect(await screen.findByText("4 triangles")).toHaveAttribute("title", "Dimensions: 70 × 20 × 0");
  });

  it("shows a parse error for a malformed 3MF file", async () => {
    fetchMock.mockResolvedValue(new Response("not a zip"));
    renderViewer("/models/broken.3mf");
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(screen.queryByText(/triangles/)).not.toBeInTheDocument();
  });

  it("closes the viewer to its task regardless of browser history", async () => {
    const user = userEvent.setup();
    fetchMock.mockResolvedValue(new Response(new Uint8Array(10)));
    window.history.replaceState(null, "", "/task/@other");
    window.history.pushState(null, "", "/task/@abc/view");
    renderViewer("/home/user/models/sample.stl");

    const closeBtn = screen.getByRole("button", { name: "Close view" });
    await user.click(closeBtn);
    await waitFor(() => expect(window.location.pathname).toBe("/task/@abc+test"));
  });

  it("navigates back when Escape key is pressed", async () => {
    const user = userEvent.setup();
    fetchMock.mockResolvedValue(new Response(new Uint8Array(10)));
    renderViewer("/home/user/models/sample.stl");

    await user.keyboard("{Escape}");
    expect(window.location.pathname).toBe("/task/@abc+test");
  });

  it("displays an error alert when the file fails to load", async () => {
    fetchMock.mockResolvedValue(
      new Response(JSON.stringify({ error: { message: "File not found" } }), { status: 404 }),
    );
    renderViewer("/missing.stl");

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("File not found");
  });
});
