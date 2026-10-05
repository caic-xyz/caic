// Reusable prompt input with image support: paste, drag & drop, attach button, and preview strip.

import { createEffect, createSignal, on, onCleanup, For, Show, type JSX } from "solid-js";
import AttachIcon from "@material-symbols/svg-400/outlined/attach_file.svg?solid";
import CameraIcon from "@material-symbols/svg-400/outlined/photo_camera.svg?solid";
import ImageIcon from "@material-symbols/svg-400/outlined/image.svg?solid";
import ScreenshotIcon from "@material-symbols/svg-400/outlined/screenshot_monitor.svg?solid";

import type { ImageConstraints } from "@sdk/types.gen";

import { captureScreen, imagesFromClipboard, type DraftImage } from "../images";
import AutoResizeTextarea from "./AutoResizeTextarea";
import Button from "./Button";
import CameraCapture from "./CameraCapture";
import Dropdown from "./Dropdown";
import styles from "./PromptInput.module.css";

interface Props {
  value: string;
  onInput: (value: string) => void;
  onSubmit?: () => void;
  onKeyDown?: (event: KeyboardEvent) => void;
  placeholder?: string;
  disabled?: boolean;
  class?: string;
  tabIndex?: number;
  ref?: (el: HTMLDivElement) => void;
  "data-testid"?: string;
  // Image support
  supportsImages?: boolean;
  images: DraftImage[];
  imageConstraints: ImageConstraints | null;
  imageGeneration: number;
  onAddImages: (blobs: Blob[]) => void;
  onRemoveImage: (image: DraftImage) => void;
  /** Element rendered inside the text field (trailing icon, like Android). */
  sendButton?: JSX.Element;
  /** Elements rendered outside the text field row (action buttons). */
  children?: JSX.Element;
}

export default function PromptInput(props: Props) {
  const [dragging, setDragging] = createSignal(false);
  const [menuOpen, setMenuOpen] = createSignal(false);
  const [menuFlipped, setMenuFlipped] = createSignal(false);
  const [cameraOpen, setCameraOpen] = createSignal(false);

  const [imageError, setImageError] = createSignal("");
  let captureController = new AbortController();
  let active = true;
  onCleanup(() => {
    active = false;
    captureController.abort();
  });
  createEffect(
    on(
      () => props.imageGeneration,
      () => {
        captureController.abort();
        captureController = new AbortController();
        setCameraOpen(false);
      },
      { defer: true },
    ),
  );

  function adopt(blobs: Blob[]) {
    if (!active || props.disabled || !props.supportsImages) return;
    props.onAddImages(blobs);
    setImageError("");
  }

  function addImages(blobs: Blob[]) {
    if (!blobs.length) return;
    try {
      adopt(blobs);
    } catch (err) {
      setImageError(err instanceof Error ? err.message : "Could not attach images.");
    }
  }

  function handlePaste(e: ClipboardEvent) {
    if (props.supportsImages) addImages(imagesFromClipboard(e));
  }

  function handleDragOver(e: DragEvent) {
    if (!props.supportsImages || props.disabled || !props.imageConstraints) return;
    e.preventDefault();
    setDragging(true);
  }

  function handleDragLeave(e: DragEvent) {
    const wrapper = e.currentTarget as HTMLElement;
    if (!wrapper.contains(e.relatedTarget as Node)) setDragging(false);
  }

  function handleDrop(e: DragEvent) {
    e.preventDefault();
    setDragging(false);
    if (e.dataTransfer?.files.length) addImages(Array.from(e.dataTransfer.files));
  }

  let fileInputRef!: HTMLInputElement;
  function handleFileChange() {
    if (fileInputRef.files?.length) addImages(Array.from(fileInputRef.files));
    fileInputRef.value = "";
  }

  function handleAttachClick() {
    setMenuOpen(!menuOpen());
  }

  function handleChooseFile() {
    setMenuOpen(false);
    fileInputRef.click();
  }

  let cameraGeneration = 0;
  function handleTakePhoto() {
    cameraGeneration = props.imageGeneration;
    setMenuOpen(false);
    setCameraOpen(true);
  }

  async function handleScreenshot() {
    setMenuOpen(false);
    const signal = captureController.signal;
    const generation = props.imageGeneration;
    try {
      const img = await captureScreen(signal);
      if (img && !signal.aborted && generation === props.imageGeneration) addImages([img]);
    } catch (err) {
      if (active && !signal.aborted)
        setImageError(err instanceof Error ? err.message : "Could not capture the screen.");
    }
  }

  function handleCameraCapture(img: Blob) {
    if (cameraGeneration !== props.imageGeneration) throw new Error("This attachment draft is closed.");
    adopt([img]);
  }

  return (
    // eslint-disable-next-line jsx-a11y/no-static-element-interactions -- drag-and-drop target for file attachment
    <div
      class={`${styles.container}${dragging() ? ` ${styles.dragOver}` : ""}`}
      onDragOver={handleDragOver}
      onDragLeave={handleDragLeave}
      onDrop={handleDrop}
    >
      <div class={styles.row}>
        <AutoResizeTextarea
          ref={(el) => {
            el.addEventListener("paste", handlePaste);
            props.ref?.(el);
          }}
          value={props.value}
          onInput={props.onInput}
          onSubmit={props.onSubmit}
          onKeyDown={props.onKeyDown}
          placeholder={props.placeholder}
          disabled={props.disabled}
          class={props.class}
          tabIndex={props.tabIndex}
          data-testid={props["data-testid"]}
          spacerClass={styles.spacer}
        />
        <div class={styles.trailing}>
          <Show when={props.supportsImages}>
            <input
              ref={(el) => {
                fileInputRef = el;
              }}
              type="file"
              multiple
              accept={props.imageConstraints?.allowedMediaTypes.join(",") ?? ""}
              class={styles.hiddenFileInput}
              onChange={handleFileChange}
            />
            <Dropdown
              open={menuOpen()}
              onOpenChange={setMenuOpen}
              class={styles.attachWrap}
              content={
                <div
                  class={`${styles.attachMenu}${menuFlipped() ? ` ${styles.attachMenuFlipped}` : ""}`}
                  role="menu"
                  ref={(el) => {
                    requestAnimationFrame(() => {
                      const rect = el.getBoundingClientRect();
                      setMenuFlipped(rect.top < 0);
                    });
                  }}
                >
                  <button class={styles.menuItem} role="menuitem" onClick={handleTakePhoto}>
                    <CameraIcon width="1.1em" height="1.1em" />
                    Take photo
                  </button>
                  <Show when={!!navigator.mediaDevices?.getDisplayMedia}>
                    <button
                      class={styles.menuItem}
                      role="menuitem"
                      onClick={handleScreenshot}
                      data-testid="screenshot-menu-item"
                    >
                      <ScreenshotIcon width="1.1em" height="1.1em" />
                      Screenshot
                    </button>
                  </Show>
                  <button class={styles.menuItem} role="menuitem" onClick={handleChooseFile}>
                    <ImageIcon width="1.1em" height="1.1em" />
                    Choose file
                  </button>
                </div>
              }
            >
              <Button
                type="button"
                variant="gray"
                disabled={props.disabled || !props.imageConstraints}
                title={
                  props.imageConstraints
                    ? "Attach images"
                    : "Image attachments are unavailable until server limits load"
                }
                aria-label="Attach images"
                onClick={handleAttachClick}
                data-testid="attach-images"
              >
                <AttachIcon width="1.2em" height="1.2em" />
              </Button>
            </Dropdown>
          </Show>
          {props.sendButton}
        </div>
      </div>
      <Show when={props.children}>
        <div class={styles.actionRow}>{props.children}</div>
      </Show>
      <Show when={imageError()}>
        <p role="alert" class={styles.imageError}>
          {imageError()}
        </p>
      </Show>
      <Show when={props.supportsImages && !props.imageConstraints}>
        <p role="status" class={styles.imageError}>
          Image attachments are unavailable until server limits load.
        </p>
      </Show>
      <Show when={props.images.length > 0}>
        <div class={styles.imagePreviewRow}>
          <For each={props.images}>
            {(img) => (
              <div class={styles.imageThumb}>
                <img src={img.previewURL} alt="attached" />
                <button class={styles.imageRemove} onClick={() => props.onRemoveImage(img)} aria-label="Remove">
                  &times;
                </button>
              </div>
            )}
          </For>
        </div>
      </Show>
      <Show when={cameraOpen()}>
        <CameraCapture onCapture={handleCameraCapture} onClose={() => setCameraOpen(false)} />
      </Show>
    </div>
  );
}
