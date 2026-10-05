// Tests for the PromptInput component.

import { describe, it } from "node:test";
import { expect, vi } from "@tests/expect";
import { render, fireEvent } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";

import type { DraftImage } from "../images";
const imageConstraints = {
  allowedMediaTypes: ["image/png", "image/jpeg"],
  maxImageBytes: 10485760,
  maxPromptImageBytes: 20971520,
};

import PromptInput from "./PromptInput";
import { bindPromptSubmitShortcut } from "./promptSubmitShortcut";

const fakeImage: DraftImage = { id: "test", blob: new Blob(["image"], { type: "image/png" }), previewURL: "blob:test" };

function renderPromptWithCamera(onSubmit: () => void) {
  return render(() => (
    <form
      ref={bindPromptSubmitShortcut}
      onSubmit={(event) => {
        event.preventDefault();
        onSubmit();
      }}
    >
      <PromptInput
        value="take a photo"
        onInput={() => {}}
        images={[]}
        onAddImages={() => {}}
        onRemoveImage={() => {}}
        imageConstraints={imageConstraints}
        imageGeneration={0}
        supportsImages={true}
        sendButton={<button type="submit">Send</button>}
      />
    </form>
  ));
}

describe("PromptInput", () => {
  it("renders textarea with placeholder", () => {
    const { getByRole } = render(() => (
      <PromptInput
        value=""
        onInput={() => {}}
        images={[]}
        onAddImages={() => {}}
        onRemoveImage={() => {}}
        imageConstraints={imageConstraints}
        imageGeneration={0}
        placeholder="Describe a task..."
      />
    ));
    expect(getByRole("textbox")).toHaveAttribute("data-placeholder", "Describe a task...");
  });

  it("calls onSubmit on Enter", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const { getByRole } = render(() => (
      <PromptInput
        value=""
        onInput={() => {}}
        onSubmit={onSubmit}
        images={[]}
        onAddImages={() => {}}
        onRemoveImage={() => {}}
        imageConstraints={imageConstraints}
        imageGeneration={0}
      />
    ));
    getByRole("textbox").focus();
    await user.keyboard("{Enter}");
    expect(onSubmit).toHaveBeenCalledOnce();
  });

  it("keeps Ctrl+Enter inside the camera dialog from submitting its containing form", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const { getByRole } = renderPromptWithCamera(onSubmit);

    await user.click(getByRole("button", { name: "Attach images" }));
    await user.click(getByRole("menuitem", { name: "Take photo" }));
    const dialog = getByRole("dialog");
    expect(dialog).toHaveAttribute("open");
    getByRole("button", { name: "Cancel" }).focus();
    await user.keyboard("{Control>}{Enter}{/Control}");

    expect(onSubmit).not.toHaveBeenCalled();
  });

  it("keeps plain Enter on camera Cancel from submitting its containing form", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const { getByRole, queryByRole } = renderPromptWithCamera(onSubmit);

    await user.click(getByRole("button", { name: "Attach images" }));
    await user.click(getByRole("menuitem", { name: "Take photo" }));
    getByRole("button", { name: "Cancel" }).focus();
    await user.keyboard("{Enter}");

    expect(queryByRole("dialog")).not.toBeInTheDocument();
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it("keeps plain Enter on Switch camera from submitting its containing form", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const mediaDevices = Object.getOwnPropertyDescriptor(navigator, "mediaDevices");
    const play = vi.spyOn(window.HTMLMediaElement.prototype, "play").mockResolvedValue();
    Object.defineProperty(navigator, "mediaDevices", {
      configurable: true,
      value: {
        getUserMedia: async () => ({ getTracks: () => [] }),
        enumerateDevices: async () => [{ kind: "videoinput" }, { kind: "videoinput" }],
      },
    });
    try {
      const { getByRole } = renderPromptWithCamera(onSubmit);
      await user.click(getByRole("button", { name: "Attach images" }));
      await user.click(getByRole("menuitem", { name: "Take photo" }));
      const switchCamera = await vi.waitFor(() => getByRole("button", { name: "Switch camera" }));
      switchCamera.focus();
      await user.keyboard("{Enter}");

      expect(getByRole("dialog")).toHaveAttribute("open");
      expect(onSubmit).not.toHaveBeenCalled();
    } finally {
      play.mockRestore();
      if (mediaDevices) Object.defineProperty(navigator, "mediaDevices", mediaDevices);
      else Reflect.deleteProperty(navigator, "mediaDevices");
    }
  });

  it("shows attach button when supportsImages is true", () => {
    const { getByRole } = render(() => (
      <PromptInput
        value=""
        onInput={() => {}}
        images={[]}
        onAddImages={() => {}}
        onRemoveImage={() => {}}
        imageConstraints={imageConstraints}
        imageGeneration={0}
        supportsImages={true}
      />
    ));
    expect(getByRole("button", { name: "Attach images" })).toBeInTheDocument();
  });

  it("hides attach button when supportsImages is false", () => {
    const { queryByRole } = render(() => (
      <PromptInput
        value=""
        onInput={() => {}}
        images={[]}
        onAddImages={() => {}}
        onRemoveImage={() => {}}
        imageConstraints={imageConstraints}
        imageGeneration={0}
        supportsImages={false}
      />
    ));
    expect(queryByRole("button", { name: "Attach images" })).not.toBeInTheDocument();
  });

  it("shows image preview when images is non-empty", () => {
    const { getAllByAltText } = render(() => (
      <PromptInput
        value=""
        onInput={() => {}}
        images={[fakeImage]}
        onAddImages={() => {}}
        onRemoveImage={() => {}}
        imageConstraints={imageConstraints}
        imageGeneration={0}
      />
    ));
    expect(getAllByAltText("attached")).toHaveLength(1);
  });

  it("calls onImagesChange to remove an image when remove button clicked", async () => {
    const user = userEvent.setup();
    const onImagesChange = vi.fn();
    const { getByRole } = render(() => (
      <PromptInput
        value=""
        onInput={() => {}}
        images={[fakeImage]}
        onAddImages={onImagesChange}
        onRemoveImage={onImagesChange}
        imageConstraints={imageConstraints}
        imageGeneration={0}
      />
    ));
    await user.click(getByRole("button", { name: "Remove" }));
    expect(onImagesChange).toHaveBeenCalledWith(fakeImage);
  });

  it("renders children alongside textarea", () => {
    const { getByText } = render(() => (
      <PromptInput
        value=""
        onInput={() => {}}
        images={[]}
        onAddImages={() => {}}
        onRemoveImage={() => {}}
        imageConstraints={imageConstraints}
        imageGeneration={0}
      >
        <button>Send</button>
      </PromptInput>
    ));
    expect(getByText("Send")).toBeInTheDocument();
  });

  it("adds dragOver class on dragover and removes on dragleave", () => {
    const { container } = render(() => (
      <PromptInput
        value=""
        onInput={() => {}}
        images={[]}
        onAddImages={() => {}}
        onRemoveImage={() => {}}
        imageConstraints={imageConstraints}
        imageGeneration={0}
        supportsImages={true}
      />
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
      <PromptInput
        value=""
        onInput={() => {}}
        images={[]}
        onAddImages={onImagesChange}
        onRemoveImage={onImagesChange}
        imageConstraints={imageConstraints}
        imageGeneration={0}
        supportsImages={true}
      />
    ));
    const wrapper = container.firstElementChild as HTMLElement;
    const file = new File(["fake"], "test.png", { type: "image/png" });
    // Mock arrayBuffer to return valid data.
    file.arrayBuffer = () => Promise.resolve(new Uint8Array([137, 80]).buffer);
    const dataTransfer = { files: [file], items: [], types: ["Files"] };
    fireEvent.drop(wrapper, { dataTransfer });
    // Wait for async file processing.
    await vi.waitFor(() => expect(onImagesChange).toHaveBeenCalled());
    const imgs = onImagesChange.mock.calls[0][0] as Blob[];
    expect(imgs).toHaveLength(1);
    expect(imgs[0].type).toBe("image/png");
  });

  it("calls onImagesChange on paste with image data", async () => {
    const onImagesChange = vi.fn();
    const { getByRole } = render(() => (
      <PromptInput
        value=""
        onInput={() => {}}
        images={[]}
        onAddImages={onImagesChange}
        onRemoveImage={onImagesChange}
        imageConstraints={imageConstraints}
        imageGeneration={0}
        supportsImages={true}
      />
    ));
    const textarea = getByRole("textbox");
    const file = new File(["fake"], "paste.png", { type: "image/png" });
    file.arrayBuffer = () => Promise.resolve(new Uint8Array([137, 80]).buffer);
    const clipboardData = {
      items: [{ kind: "file", type: "image/png", getAsFile: () => file }],
    };
    fireEvent.paste(textarea, { clipboardData });
    await vi.waitFor(() => expect(onImagesChange).toHaveBeenCalled());
    const imgs = onImagesChange.mock.calls[0][0] as Blob[];
    expect(imgs).toHaveLength(1);
    expect(imgs[0].type).toBe("image/png");
  });

  it("opens attach menu on attach button click", async () => {
    const user = userEvent.setup();
    const { getByRole, getByText, queryByText } = render(() => (
      <PromptInput
        value=""
        onInput={() => {}}
        images={[]}
        onAddImages={() => {}}
        onRemoveImage={() => {}}
        imageConstraints={imageConstraints}
        imageGeneration={0}
        supportsImages={true}
      />
    ));
    expect(queryByText("Take photo")).not.toBeInTheDocument();
    await user.click(getByRole("button", { name: "Attach images" }));
    expect(getByText("Take photo")).toBeInTheDocument();
    // Screenshot only shown when getDisplayMedia is available (not in jsdom).
    expect(getByText("Choose file")).toBeInTheDocument();
  });

  it("closes attach menu and opens file picker on Choose file", async () => {
    const user = userEvent.setup();
    const { getByRole, getByText, queryByText } = render(() => (
      <PromptInput
        value=""
        onInput={() => {}}
        images={[]}
        onAddImages={() => {}}
        onRemoveImage={() => {}}
        imageConstraints={imageConstraints}
        imageGeneration={0}
        supportsImages={true}
      />
    ));
    await user.click(getByRole("button", { name: "Attach images" }));
    const fileInput = document.querySelector("input[type=file]") as HTMLInputElement;
    const clickSpy = vi.spyOn(fileInput, "click").mockImplementation(() => {});
    await user.click(getByText("Choose file"));
    expect(queryByText("Choose file")).not.toBeInTheDocument();
    expect(clickSpy).toHaveBeenCalled();
    clickSpy.mockRestore();
  });

  it("toggles attach menu closed on second click", async () => {
    const user = userEvent.setup();
    const { getByRole, getByText, queryByText } = render(() => (
      <PromptInput
        value=""
        onInput={() => {}}
        images={[]}
        onAddImages={() => {}}
        onRemoveImage={() => {}}
        imageConstraints={imageConstraints}
        imageGeneration={0}
        supportsImages={true}
      />
    ));
    await user.click(getByRole("button", { name: "Attach images" }));
    expect(getByText("Take photo")).toBeInTheDocument();
    await user.click(getByRole("button", { name: "Attach images" }));
    expect(queryByText("Take photo")).not.toBeInTheDocument();
  });
});
