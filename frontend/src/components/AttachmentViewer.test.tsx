// Tests the prompt-image pane image source, header link, and failure notice.
import { afterEach, describe, it } from "node:test";
import { cleanup, fireEvent, render, screen } from "@solidjs/testing-library";
import { Route, Router } from "@solidjs/router";
import { expect } from "@tests/expect";
import AttachmentViewer from "./AttachmentViewer";

const hash = "a".repeat(64);

function fixture() {
  window.history.replaceState(null, "", `/task/@abc/view?attachment=${hash}`);
  return render(() => (
    <Router>
      <Route path="/*" component={() => <AttachmentViewer taskId="abc" taskPath="/task/@abc" hash={hash} />} />
    </Router>
  ));
}

afterEach(cleanup);

describe("AttachmentViewer", () => {
  it("shows the full image and links to the raw bytes", () => {
    fixture();
    const image = screen.getByRole("img", { name: "attachment" });
    expect(image.getAttribute("src")).toBe(`/api/caic/v1/tasks/abc/attachments/${hash}?w=1280`);
    const raw = screen.getByRole("link", { name: "full image" });
    expect(raw.getAttribute("href")).toBe(`/api/caic/v1/tasks/abc/attachments/${hash}`);
    expect(raw.getAttribute("target")).toBe("_blank");
  });

  it("replaces a failed image with an alert", () => {
    fixture();
    fireEvent.error(screen.getByRole("img", { name: "attachment" }));
    expect(screen.queryByRole("img", { name: "attachment" })).not.toBeInTheDocument();
    expect(screen.getByRole("alert").textContent).toBe("Image unavailable");
  });
});
