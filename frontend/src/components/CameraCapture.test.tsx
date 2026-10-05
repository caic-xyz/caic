// Tests camera permission and Blob capture completion after dialog disposal.

import { afterEach, it } from "node:test";
import { fireEvent, render } from "@solidjs/testing-library";
import { expect, vi } from "@tests/expect";
import CameraCapture from "./CameraCapture";

let originalDevices: PropertyDescriptor | undefined;
afterEach(() => {
  vi.restoreAllMocks();
  if (originalDevices) Object.defineProperty(navigator, "mediaDevices", originalDevices);
  else Reflect.deleteProperty(navigator, "mediaDevices");
});
function camera(getUserMedia: () => Promise<MediaStream>) {
  originalDevices = Object.getOwnPropertyDescriptor(navigator, "mediaDevices");
  Object.defineProperty(navigator, "mediaDevices", {
    configurable: true,
    value: { getUserMedia, enumerateDevices: async () => [] },
  });
  vi.spyOn(window.HTMLMediaElement.prototype, "play").mockResolvedValue();
}

it("stops a camera stream whose permission resolves after the dialog is closed", async () => {
  let resolve!: (stream: MediaStream) => void;
  const permission = new Promise<MediaStream>((r) => {
    resolve = r;
  });
  camera(() => permission);
  const stop = vi.fn();
  const view = render(() => <CameraCapture onCapture={() => {}} onClose={() => {}} />);
  view.unmount();
  resolve({ getTracks: () => [{ stop }] } as unknown as MediaStream);
  await permission;
  expect(stop).toHaveBeenCalledOnce();
});

it("does not adopt a Blob whose encoding completes after camera cancellation", async () => {
  const stop = vi.fn();
  camera(async () => ({ getTracks: () => [{ stop }] }) as unknown as MediaStream);
  vi.spyOn(window.HTMLCanvasElement.prototype, "getContext").mockReturnValue({
    drawImage: () => {},
  } as unknown as CanvasRenderingContext2D);
  let complete!: BlobCallback;
  const encode = vi.spyOn(window.HTMLCanvasElement.prototype, "toBlob").mockImplementation((callback) => {
    complete = callback;
  });
  const onCapture = vi.fn();
  const view = render(() => <CameraCapture onCapture={onCapture} onClose={() => {}} />);
  const video = view.container.querySelector("video");
  if (!video) throw new Error("Camera preview missing");
  Object.defineProperties(video, { videoWidth: { value: 100 }, videoHeight: { value: 100 } });
  await vi.waitFor(() => expect(view.getByRole("button", { name: "Take photo" })).toBeEnabled());
  fireEvent.click(view.getByRole("button", { name: "Take photo" }));
  expect(encode).toHaveBeenCalledOnce();
  view.unmount();
  complete(new Blob(["image"], { type: "image/jpeg" }));
  await Promise.resolve();
  expect(onCapture).not.toHaveBeenCalled();
  expect(stop).toHaveBeenCalledOnce();
});

it("shows rejected attachment admission without closing the camera, allowing retry", async () => {
  camera(async () => ({ getTracks: () => [] }) as unknown as MediaStream);
  vi.spyOn(window.HTMLCanvasElement.prototype, "getContext").mockReturnValue({
    drawImage: () => {},
  } as unknown as CanvasRenderingContext2D);
  vi.spyOn(window.HTMLCanvasElement.prototype, "toBlob").mockImplementation((callback) =>
    callback(new Blob(["image"], { type: "image/jpeg" })),
  );
  const close = vi.fn();
  const capture = vi.fn().mockImplementationOnce(() => {
    throw new Error("Attachments must total 20 MiB or less.");
  });
  const view = render(() => <CameraCapture onCapture={capture} onClose={close} />);
  const video = view.container.querySelector("video");
  if (!video) throw new Error("Camera preview missing");
  Object.defineProperties(video, { videoWidth: { value: 100 }, videoHeight: { value: 100 } });
  await vi.waitFor(() => expect(view.getByRole("button", { name: "Take photo" })).toBeEnabled());
  fireEvent.click(view.getByRole("button", { name: "Take photo" }));
  await vi.waitFor(() => expect(view.getByRole("alert")).toHaveTextContent("Attachments must total 20 MiB or less."));
  expect(close).not.toHaveBeenCalled();
  fireEvent.click(view.getByRole("button", { name: "Take photo" }));
  await vi.waitFor(() => expect(close).toHaveBeenCalledOnce());
  view.unmount();
});
