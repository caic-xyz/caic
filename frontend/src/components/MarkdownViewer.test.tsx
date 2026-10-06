// Tests markdown file loading, directory-relative links and images, in-app navigation, and failure states.

import { afterEach, beforeEach, describe, it } from "node:test";
import { expect, vi } from "@tests/expect";
import { Route, Router } from "@solidjs/router";
import { cleanup, render, screen } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";

import MarkdownViewer from "./MarkdownViewer";

const textHeaders = { "Content-Type": "text/plain; charset=utf-8" };

function renderViewer(path: string) {
  window.history.replaceState(null, "", `/task/@abc/view?path=${encodeURIComponent(path)}`);
  return render(() => (
    <Router>
      <Route path="/*" component={() => <MarkdownViewer taskId="abc" taskPath="/task/@abc+test" path={path} />} />
    </Router>
  ));
}

describe("MarkdownViewer", () => {
  const fetchMock = vi.spyOn(globalThis, "fetch");

  beforeEach(() => {
    fetchMock.mockReset();
  });

  afterEach(() => {
    cleanup();
  });

  it("requests the file and resolves links and images against its directory", async () => {
    fetchMock.mockResolvedValue(
      new Response("# Guide\n\n[Next](next.md) [Source](../src/a.go:3) ![Shot](../shots/a.png)\n", {
        headers: textHeaders,
      }),
    );
    renderViewer("/home/user/docs/guide.md");

    expect(await screen.findByRole("heading", { name: "Guide" })).toBeInTheDocument();
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/caic/v1/tasks/abc/file?path=%2Fhome%2Fuser%2Fdocs%2Fguide.md");
    const next = screen.getByRole("link", { name: "Next" });
    expect(next).toHaveAttribute("href", "/task/@abc/view?path=%2Fhome%2Fuser%2Fdocs%2Fnext.md");
    expect(next).not.toHaveAttribute("target");
    const source = screen.getByRole("link", { name: "Source" });
    expect(source).toHaveAttribute("href", "/api/caic/v1/tasks/abc/file?path=%2Fhome%2Fuser%2Fsrc%2Fa.go");
    expect(source).toHaveAttribute("target", "_blank");
    expect(screen.getByRole("img", { name: "Shot" })).toHaveAttribute(
      "src",
      "/api/caic/v1/tasks/abc/file?path=%2Fhome%2Fuser%2Fshots%2Fa.png",
    );
  });

  it("navigates in the app when a markdown link is clicked", async () => {
    fetchMock.mockResolvedValue(new Response("[Next](next.md)\n", { headers: textHeaders }));
    renderViewer("/home/user/docs/guide.md");
    await userEvent.setup().click(await screen.findByRole("link", { name: "Next" }));

    expect(window.location.pathname).toBe("/task/@abc/view");
    expect(window.location.search).toBe("?path=%2Fhome%2Fuser%2Fdocs%2Fnext.md");
  });

  it("shows front matter as a labeled source block above the body", async () => {
    fetchMock.mockResolvedValue(
      new Response("---\nname: widget\ndescription: Render widgets\n---\n# Widget\n", { headers: textHeaders }),
    );
    renderViewer("/home/user/SKILL.md");

    expect(await screen.findByRole("heading", { name: "Widget" })).toBeInTheDocument();
    expect(screen.getByText("Front matter (YAML)")).toBeInTheDocument();
    expect(screen.getByText(/description: Render widgets/)).toBeInTheDocument();
    expect(screen.queryByRole("separator")).not.toBeInTheDocument();
  });

  it("reports the server's error message", async () => {
    fetchMock.mockResolvedValue(
      Response.json({ error: { code: "NOT_FOUND", message: "file not found" } }, { status: 404 }),
    );
    renderViewer("/missing.md");

    expect(await screen.findByRole("alert")).toHaveTextContent("file not found");
  });

  it("rejects a file the endpoint serves as a download", async () => {
    fetchMock.mockResolvedValue(new Response("\x89PNG", { headers: { "Content-Type": "image/png" } }));
    renderViewer("/shot.md");

    expect(await screen.findByRole("alert")).toHaveTextContent("not a text file");
  });

  it("cuts a file past the size limit and says so", async () => {
    fetchMock.mockResolvedValue(new Response(`# Big\n\n${"x".repeat(1.5 * 1024 * 1024)}\n`, { headers: textHeaders }));
    renderViewer("/big.md");

    expect(await screen.findByText(/File truncated/)).toBeInTheDocument();
  });
});
