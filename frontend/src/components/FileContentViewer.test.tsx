// Tests unified file previews, safe text and image rendering, relative Markdown links, and failures.

import { afterEach, beforeEach, describe, it } from "node:test";
import { expect, vi } from "@tests/expect";
import { Route, Router } from "@solidjs/router";
import { cleanup, render, screen } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";

import FileContentViewer from "./FileContentViewer";

const textHeaders = { "Content-Type": "text/plain; charset=utf-8" };

function renderViewer(path: string) {
  window.history.replaceState(null, "", `/task/@abc/view?path=${encodeURIComponent(path)}`);
  return render(() => (
    <Router>
      <Route path="/*" component={() => <FileContentViewer taskId="abc" taskPath="/task/@abc+test" path={path} />} />
    </Router>
  ));
}

describe("FileContentViewer", () => {
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
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/caic/v1/tasks/abc/raw?path=%2Fhome%2Fuser%2Fdocs%2Fguide.md");
    const next = screen.getByRole("link", { name: "Next" });
    expect(next).toHaveAttribute("href", "/task/@abc/view?path=%2Fhome%2Fuser%2Fdocs%2Fnext.md");
    expect(next).not.toHaveAttribute("target");
    const source = screen.getByRole("link", { name: "Source" });
    expect(source).toHaveAttribute("href", "/task/@abc/view?path=%2Fhome%2Fuser%2Fsrc%2Fa.go");
    expect(source).not.toHaveAttribute("target");
    expect(screen.getByRole("img", { name: "Shot" })).toHaveAttribute(
      "src",
      "/api/caic/v1/tasks/abc/raw?path=%2Fhome%2Fuser%2Fshots%2Fa.png",
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

  it("uses sniffed image type even when the filename says Markdown", async () => {
    fetchMock.mockResolvedValue(new Response("\x89PNG", { headers: { "Content-Type": "image/png" } }));
    renderViewer("/shot.md");
    expect(await screen.findByRole("img", { name: "/shot.md" })).toHaveAttribute(
      "src",
      "/api/caic/v1/tasks/abc/raw?path=%2Fshot.md",
    );
  });

  it("renders source code and active documents as literal text, not Markdown or HTML", async () => {
    fetchMock.mockResolvedValue(
      new Response("<script>alert(1)</script>\n# Heading", { headers: { "Content-Type": "text/html" } }),
    );
    const { container } = renderViewer("/page.html");
    expect(await screen.findByText(/<script>alert/)).toBeInTheDocument();
    expect(container.querySelector("script")).toBeNull();
    expect(screen.queryByRole("heading")).not.toBeInTheDocument();
  });

  it("sanitizes active HTML and executable URLs inside a Markdown file", async () => {
    fetchMock.mockResolvedValue(
      new Response(
        '# Notes\n\n<img src=x onerror="alert(1)"><script>alert(2)</script><style>body{display:none}</style><form method="post" action="/api/caic/v1/tasks"><button formaction="/api/caic/v1/tasks/abc/purge" form="target">Submit</button></form>\n\n[Run](javascript:alert%283%29)',
        { headers: { "Content-Type": "text/html" } },
      ),
    );
    const { container } = renderViewer("/notes.md");
    expect(await screen.findByRole("heading", { name: "Notes" })).toBeInTheDocument();
    expect(
      container.querySelector("[onerror], script, style, form, [form], [formaction], a[href^='javascript:']"),
    ).toBeNull();
  });

  it("offers raw access for unsupported binaries without retaining their body", async () => {
    let cancelled = false;
    fetchMock.mockResolvedValue(
      new Response(
        new ReadableStream({
          cancel() {
            cancelled = true;
          },
        }),
        { headers: { "Content-Type": "application/pdf" } },
      ),
    );
    renderViewer("/report.pdf");
    expect(await screen.findByText(/No preview/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "raw file" })).toHaveAttribute(
      "href",
      "/api/caic/v1/tasks/abc/raw?path=%2Freport.pdf",
    );
    expect(cancelled).toBe(true);
  });

  it("cuts a file past the size limit and says so", async () => {
    fetchMock.mockResolvedValue(new Response(`# Big\n\n${"x".repeat(1.5 * 1024 * 1024)}\n`, { headers: textHeaders }));
    renderViewer("/big.md");

    expect(await screen.findByText(/File truncated/)).toBeInTheDocument();
  });
});
