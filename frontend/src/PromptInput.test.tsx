// Tests for the PromptInput component.
import { describe, it, expect, vi } from "vitest";
import { render, fireEvent } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import type { ImageData as APIImageData } from "@sdk/types.gen";
import PromptInput from "./PromptInput";

const fakeImage: APIImageData = { mediaType: "image/png", data: "iVBOR" };

describe("PromptInput", () => {
  it("renders textarea with placeholder", () => {
    const { getByPlaceholderText } = render(() => (
      <PromptInput value="" onInput={() => {}} images={[]} onImagesChange={() => {}} placeholder="Describe a task..." />
    ));
    expect(getByPlaceholderText("Describe a task...")).toBeInTheDocument();
  });

  it("calls onSubmit on Enter", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const { getByRole } = render(() => (
      <PromptInput value="" onInput={() => {}} onSubmit={onSubmit} images={[]} onImagesChange={() => {}} />
    ));
    getByRole("textbox").focus();
    await user.keyboard("{Enter}");
    expect(onSubmit).toHaveBeenCalledOnce();
  });

  it("shows attach button when supportsImages is true", () => {
    const { getByTitle } = render(() => (
      <PromptInput value="" onInput={() => {}} images={[]} onImagesChange={() => {}} supportsImages={true} />
    ));
    expect(getByTitle("Attach images")).toBeInTheDocument();
  });

  it("hides attach button when supportsImages is false", () => {
    const { queryByTitle } = render(() => (
      <PromptInput value="" onInput={() => {}} images={[]} onImagesChange={() => {}} supportsImages={false} />
    ));
    expect(queryByTitle("Attach images")).not.toBeInTheDocument();
  });

  it("shows image preview when images is non-empty", () => {
    const { getAllByAltText } = render(() => (
      <PromptInput value="" onInput={() => {}} images={[fakeImage]} onImagesChange={() => {}} />
    ));
    expect(getAllByAltText("attached")).toHaveLength(1);
  });

  it("calls onImagesChange to remove an image when remove button clicked", async () => {
    const user = userEvent.setup();
    const onImagesChange = vi.fn();
    const { getByTitle } = render(() => (
      <PromptInput value="" onInput={() => {}} images={[fakeImage]} onImagesChange={onImagesChange} />
    ));
    await user.click(getByTitle("Remove"));
    expect(onImagesChange).toHaveBeenCalledWith([]);
  });

  it("renders children alongside textarea", () => {
    const { getByText } = render(() => (
      <PromptInput value="" onInput={() => {}} images={[]} onImagesChange={() => {}}>
        <button>Send</button>
      </PromptInput>
    ));
    expect(getByText("Send")).toBeInTheDocument();
  });

  it("adds dragOver class on dragover and removes on dragleave", () => {
    const { container } = render(() => (
      <PromptInput value="" onInput={() => {}} images={[]} onImagesChange={() => {}} supportsImages={true} />
    ));
    const wrapper = container.firstElementChild as HTMLElement;
    fireEvent.dragOver(wrapper, { dataTransfer: { files: [] } });
    expect(wrapper.className).toContain("dragOver");
    // Simulate leaving the wrapper entirely (relatedTarget outside).
    fireEvent.dragLeave(wrapper, { relatedTarget: document.body });
    expect(wrapper.className).not.toContain("dragOver");
  });

  it("calls onImagesChange on drop with image files", async () => {
    const onImagesChange = vi.fn();
    const { container } = render(() => (
      <PromptInput value="" onInput={() => {}} images={[]} onImagesChange={onImagesChange} supportsImages={true} />
    ));
    const wrapper = container.firstElementChild as HTMLElement;
    const file = new File(["fake"], "test.png", { type: "image/png" });
    // Mock arrayBuffer to return valid data.
    file.arrayBuffer = () => Promise.resolve(new Uint8Array([137, 80]).buffer);
    const dataTransfer = { files: [file], items: [], types: ["Files"] };
    fireEvent.drop(wrapper, { dataTransfer });
    // Wait for async file processing.
    await vi.waitFor(() => expect(onImagesChange).toHaveBeenCalled());
    const imgs = onImagesChange.mock.calls[0][0] as APIImageData[];
    expect(imgs).toHaveLength(1);
    expect(imgs[0].mediaType).toBe("image/png");
  });

  it("calls onImagesChange on paste with image data", async () => {
    const onImagesChange = vi.fn();
    const { getByRole } = render(() => (
      <PromptInput value="" onInput={() => {}} images={[]} onImagesChange={onImagesChange} supportsImages={true} />
    ));
    const textarea = getByRole("textbox");
    const file = new File(["fake"], "paste.png", { type: "image/png" });
    file.arrayBuffer = () => Promise.resolve(new Uint8Array([137, 80]).buffer);
    const clipboardData = {
      items: [{ kind: "file", type: "image/png", getAsFile: () => file }],
    };
    fireEvent.paste(textarea, { clipboardData });
    await vi.waitFor(() => expect(onImagesChange).toHaveBeenCalled());
    const imgs = onImagesChange.mock.calls[0][0] as APIImageData[];
    expect(imgs).toHaveLength(1);
    expect(imgs[0].mediaType).toBe("image/png");
  });

  it("opens attach menu on attach button click", async () => {
    const user = userEvent.setup();
    const { getByTitle, getByText, queryByText } = render(() => (
      <PromptInput value="" onInput={() => {}} images={[]} onImagesChange={() => {}} supportsImages={true} />
    ));
    expect(queryByText("Take photo")).not.toBeInTheDocument();
    await user.click(getByTitle("Attach images"));
    expect(getByText("Take photo")).toBeInTheDocument();
    // Screenshot only shown when getDisplayMedia is available (not in jsdom).
    expect(getByText("Choose file")).toBeInTheDocument();
  });

  it("closes attach menu and opens file picker on Choose file", async () => {
    const user = userEvent.setup();
    const { getByTitle, getByText, queryByText } = render(() => (
      <PromptInput value="" onInput={() => {}} images={[]} onImagesChange={() => {}} supportsImages={true} />
    ));
    await user.click(getByTitle("Attach images"));
    const fileInput = document.querySelector("input[type=file]") as HTMLInputElement;
    const clickSpy = vi.spyOn(fileInput, "click").mockImplementation(() => {});
    await user.click(getByText("Choose file"));
    expect(queryByText("Choose file")).not.toBeInTheDocument();
    expect(clickSpy).toHaveBeenCalled();
    clickSpy.mockRestore();
  });

  it("toggles attach menu closed on second click", async () => {
    const user = userEvent.setup();
    const { getByTitle, getByText, queryByText } = render(() => (
      <PromptInput value="" onInput={() => {}} images={[]} onImagesChange={() => {}} supportsImages={true} />
    ));
    await user.click(getByTitle("Attach images"));
    expect(getByText("Take photo")).toBeInTheDocument();
    await user.click(getByTitle("Attach images"));
    expect(queryByText("Take photo")).not.toBeInTheDocument();
  });
});
