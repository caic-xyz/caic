// Tests CAD viewer loading, model and plate metadata, failure states, and keyboard navigation.

import { afterEach, beforeEach, describe, it } from "node:test";
import { expect, vi } from "@tests/expect";
import { Route, Router } from "@solidjs/router";
import { cleanup, render, screen } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";

import CadViewer from "./CadViewer";
import { create3mfFixture, createBambu3mfFixture } from "../../tests/cad-fixture";

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

  it("loads a STEP model, parses its metrics, and displays the triangle count", async () => {
    const { readFileSync } = await import("node:fs");
    const { resolve } = await import("node:path");
    const filePath = resolve("node_modules/occt-import-js/test/testfiles/simple-basic-cube/cube.stp");
    const nodeBuf = readFileSync(filePath);

    fetchMock.mockResolvedValue(new Response(nodeBuf));
    renderViewer("/home/user/models/cube.step");

    expect(screen.getByText("/home/user/models/cube.step")).toBeInTheDocument();
    expect(await screen.findByText(/12 triangles/)).toBeInTheDocument();
  });

  it("loads 3MF metadata without WebGL", async () => {
    fetchMock.mockResolvedValue(new Response(create3mfFixture()));
    renderViewer("/models/assembly.3mf");
    expect(await screen.findByText("2 triangles")).toHaveAttribute("title", "Dimensions: 30 × 20 × 0");
  });

  it("lists printing plates without requiring WebGL", async () => {
    fetchMock.mockResolvedValue(new Response(createBambu3mfFixture()));
    renderViewer("/models/bambu.3mf");
    expect(await screen.findByRole("list", { name: "Printing plates" })).toHaveTextContent("Plate 1: Panel");
    expect(screen.getByText("Plate 2: Frame")).toBeInTheDocument();
    expect(screen.getByText("Plate 3: Spare")).toBeInTheDocument();
    expect(screen.getByText("4 triangles")).toHaveAttribute("title", "Dimensions: 70 × 20 × 0");
  });

  it("shows a parse error for a malformed 3MF file", async () => {
    fetchMock.mockResolvedValue(new Response("not a zip"));
    renderViewer("/models/broken.3mf");
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(screen.queryByText(/triangles/)).not.toBeInTheDocument();
  });

  it("navigates back to the task when back button is clicked", async () => {
    const user = userEvent.setup();
    fetchMock.mockResolvedValue(new Response(new Uint8Array(10)));
    renderViewer("/home/user/models/sample.stl");

    const backBtn = screen.getByRole("button", { name: "Close document" });
    await user.click(backBtn);
    expect(window.location.pathname).toBe("/task/@abc+test");
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
