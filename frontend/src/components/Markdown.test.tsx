// Tests Markdown prose rendering, runtime links, code copying, inline Mermaid diagrams, and raw mode toggle.

import { afterEach, beforeEach, describe, it } from "node:test";
import { expect } from "@tests/expect";
import { Route, Router } from "@solidjs/router";
import { cleanup, render, screen, waitFor } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import type { Accessor } from "solid-js";

import Markdown, { MarkdownTaskContext } from "./Markdown";
import { clearMermaidCache, renderMermaidDiagram } from "../mermaid";

function renderMarkdown(text: string, taskContext?: Accessor<string>) {
  return render(() => (
    <Router>
      <Route
        path="/*"
        component={() =>
          taskContext ? (
            <MarkdownTaskContext.Provider value={taskContext}>
              <Markdown text={text} />
            </MarkdownTaskContext.Provider>
          ) : (
            <Markdown text={text} />
          )
        }
      />
    </Router>
  ));
}

describe("Markdown", () => {
  beforeEach(() => {
    clearMermaidCache();
  });

  afterEach(() => {
    cleanup();
  });

  it("renders prose, headers, and regular code blocks with copy buttons", async () => {
    const text = "# Heading\n\nSome text.\n\n```python\nprint('hello')\n```";
    renderMarkdown(text);

    expect(screen.getByRole("heading", { name: "Heading" })).toBeInTheDocument();
    expect(screen.getByText("Some text.")).toBeInTheDocument();
    expect(screen.getByText("print('hello')")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Copy code block" })).toBeInTheDocument();
  });

  it("copies code block contents to clipboard", async () => {
    const user = userEvent.setup();
    let written = "";
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: {
        writeText: (str: string) => {
          written = str;
          return Promise.resolve();
        },
      },
    });

    renderMarkdown("```sh\necho 123\n```");

    const copyBtn = screen.getByRole("button", { name: "Copy code block" });
    await user.click(copyBtn);

    expect(written).toContain("echo 123");
  });

  it("renders Mermaid diagrams inline and allows toggling to source code", async () => {
    const user = userEvent.setup();
    const text =
      "Overview diagram:\n\n```mermaid\nsequenceDiagram\n    participant Alice\n    participant Bob\n    Alice->>Bob: Hello\n```";

    renderMarkdown(text);

    // Mermaid compilation under coverage can exceed Testing Library's one-second default.
    const toggleBtn = await screen.findByRole("button", { name: "Show code" }, { timeout: 10_000 });
    const diagram = screen.getByRole("img", { name: "Mermaid diagram" });
    expect(diagram).toBeInTheDocument();
    expect(diagram.innerHTML).toContain("<svg");
    expect(diagram.style.display).toBe("");
    expect(toggleBtn).toBeInTheDocument();

    // Toggle to source code
    await user.click(toggleBtn);
    expect(diagram.style.display).toBe("none");
    expect(screen.getByText(/Alice->>Bob: Hello/)).toBeVisible();
    expect(toggleBtn).toHaveTextContent("diagram");

    // Toggle back to diagram
    await user.click(toggleBtn);
    expect(diagram.style.display).toBe("");
    expect(toggleBtn).toHaveTextContent("code");
  });

  it("copies Mermaid source code when copy button in toolbar is clicked", async () => {
    const user = userEvent.setup();
    let copiedText = "";
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: {
        writeText: (str: string) => {
          copiedText = str;
          return Promise.resolve();
        },
      },
    });

    const text = "```mermaid\ngraph TD;\n  Start --> End;\n```";
    renderMarkdown(text);

    await screen.findByRole("button", { name: "Show code" }, { timeout: 10_000 });
    const copyBtn = screen.getByRole("button", { name: "Copy code block" });
    await user.click(copyBtn);

    expect(copiedText).toContain("Start --> End;");
  });

  it("gracefully falls back to code block when Mermaid diagram is invalid", async () => {
    const text = "```mermaid\nthis is invalid mermaid\n```";
    const { container } = renderMarkdown(text);

    await waitFor(() => expect(screen.getByText("this is invalid mermaid")).toBeVisible(), { timeout: 10_000 });
    const diagram = container.querySelector<HTMLElement>("[data-mermaid-diagram]");
    expect(diagram).not.toBeNull();
    expect(diagram?.style.display).toBe("none");
    expect(diagram?.innerHTML).toBe("");
  });

  it("hides source code and shows diagram placeholder initially to prevent flicker", () => {
    const text = "```mermaid\ngraph TD;\n  A --> B;\n```";
    const { container } = renderMarkdown(text);

    const diagram = container.querySelector<HTMLElement>("[data-mermaid-diagram]");
    const source = container.querySelector<HTMLElement>("[data-mermaid-source]");
    expect(diagram).not.toBeNull();
    expect(source).not.toBeNull();
    expect(diagram?.style.display).not.toBe("none");
    expect(source?.style.display).toBe("none");
  });

  it("renders cached Mermaid diagram synchronously into DOM on mount without waiting for effect", async () => {
    const code = "graph TD;\n  Cached1 --> Cached2;";
    await renderMermaidDiagram(code);

    const text = `\`\`\`mermaid\n${code}\n\`\`\``;
    const { container } = renderMarkdown(text);

    const diagram = container.querySelector<HTMLElement>("[data-mermaid-diagram]");
    expect(diagram).not.toBeNull();
    expect(diagram?.innerHTML).toContain("<svg");
    expect(diagram?.style.display).not.toBe("none");
  });

  it("toggles raw markdown text when raw button is clicked", async () => {
    const user = userEvent.setup();
    const text = "# Heading\n\nParagraph.";
    const { container } = renderMarkdown(text);

    expect(screen.getByRole("heading", { name: "Heading" })).toBeInTheDocument();

    const rawBtn = screen.getByRole("button", { name: "raw" });
    await user.click(rawBtn);

    expect(container.querySelector("pre")?.textContent).toBe(text);
    expect(screen.queryByRole("heading", { name: "Heading" })).not.toBeInTheDocument();

    const renderedBtn = screen.getByRole("button", { name: "rendered" });
    await user.click(renderedBtn);

    expect(screen.getByRole("heading", { name: "Heading" })).toBeInTheDocument();
  });

  it("routes in-app markdown links when MarkdownTaskContext is provided", async () => {
    renderMarkdown("See [Doc](doc.md) or [Code](src/main.go).", () => "task123");

    const docLink = screen.getByRole("link", { name: "Doc" });
    expect(docLink).toHaveAttribute("href", "/task/@task123/view?path=doc.md");
    expect(docLink).toHaveAttribute("data-app-link");

    const codeLink = screen.getByRole("link", { name: "Code" });
    expect(codeLink).toHaveAttribute("href", "/api/caic/v1/tasks/task123/file?path=src%2Fmain.go");
    expect(codeLink).toHaveAttribute("target", "_blank");
  });
});
