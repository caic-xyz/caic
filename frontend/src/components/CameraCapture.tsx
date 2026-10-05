// Camera capture dialog: opens webcam, lets user take a photo, returns a Blob, and releases delayed camera streams.
// Uses native <dialog> for built-in Escape handling, focus trapping, and backdrop.

import { createSignal, onCleanup, onMount, Show } from "solid-js";
import SwitchCameraIcon from "@material-symbols/svg-400/outlined/cameraswitch.svg?solid";

import { canvasToBlob } from "../images";

import ModalDialog from "./ModalDialog";
import styles from "./CameraCapture.module.css";

interface Props {
  onCapture: (img: Blob) => void;
  onClose: () => void;
}

export default function CameraCapture(props: Props) {
  let videoRef!: HTMLVideoElement;
  let canvasRef!: HTMLCanvasElement;
  const [stream, setStream] = createSignal<MediaStream | null>(null);
  const [error, setError] = createSignal("");
  const [facingMode, setFacingMode] = createSignal<"environment" | "user">("environment");
  const [hasMultiple, setHasMultiple] = createSignal(false);

  let active = true;
  let generation = 0;
  const [capturing, setCapturing] = createSignal(false);

  async function startCamera(facing: "environment" | "user") {
    const request = ++generation;
    // Stop any existing stream before switching.
    stream()
      ?.getTracks()
      .forEach((t) => t.stop());
    setStream(null);
    try {
      const s = await navigator.mediaDevices.getUserMedia({
        video: { facingMode: facing },
      });
      if (!active || request !== generation) {
        s.getTracks().forEach((t) => t.stop());
        return;
      }
      setStream(s);
      videoRef.srcObject = s;
      await videoRef.play();
      if (active && request === generation) setError("");
    } catch {
      if (active && request === generation) {
        stream()
          ?.getTracks()
          .forEach((t) => t.stop());
        setStream(null);
        setError("Could not access camera. Check browser permissions.");
      }
    }
  }

  onMount(async () => {
    await startCamera(facingMode());
    // Detect whether multiple cameras are available.
    try {
      const devices = await navigator.mediaDevices.enumerateDevices();
      if (active) setHasMultiple(devices.filter((d) => d.kind === "videoinput").length > 1);
    } catch {
      // Camera switching is optional when device enumeration is unavailable.
      if (active) setHasMultiple(false);
    }
  });

  onCleanup(() => {
    active = false;
    generation++;
    stream()
      ?.getTracks()
      .forEach((t) => t.stop());
  });

  function switchCamera() {
    const next = facingMode() === "environment" ? "user" : "environment";
    setFacingMode(next);
    void startCamera(next);
  }

  async function capture() {
    if (capturing()) return;
    const request = generation;
    setCapturing(true);
    try {
      const w = videoRef.videoWidth;
      const h = videoRef.videoHeight;
      canvasRef.width = w;
      canvasRef.height = h;
      const ctx = canvasRef.getContext("2d");
      if (!ctx || !w || !h) throw new Error("The camera is not ready. Try again.");
      ctx.drawImage(videoRef, 0, 0, w, h);
      const blob = await canvasToBlob(canvasRef);
      if (!active || request !== generation) return;
      props.onCapture(blob);
      props.onClose();
    } catch (err) {
      if (active && request === generation)
        setError(err instanceof Error ? err.message : "Could not capture the photo.");
    } finally {
      if (active) setCapturing(false);
    }
  }

  return (
    <ModalDialog class={styles.dialog} onClose={props.onClose}>
      <Show when={error()}>
        <p role="alert" class={styles.error}>
          {error()}
        </p>
      </Show>
      <>
        <video
          ref={(el) => {
            videoRef = el;
          }}
          class={styles.video}
          autoplay
          playsinline
          muted
        />
        <canvas
          ref={(el) => {
            canvasRef = el;
          }}
          class={styles.canvas}
        />
      </>
      <div class={styles.actions}>
        <Show when={hasMultiple() && !error()}>
          <button
            type="button"
            class={styles.switchBtn}
            onClick={switchCamera}
            title="Switch camera"
            disabled={capturing()}
          >
            <SwitchCameraIcon width="1.4em" height="1.4em" />
          </button>
        </Show>
        <button
          type="button"
          class={styles.captureBtn}
          onClick={capture}
          title="Take photo"
          disabled={capturing() || !stream()}
        />
        <button type="button" class={styles.closeBtn} onClick={() => props.onClose()}>
          Cancel
        </button>
      </div>
    </ModalDialog>
  );
}
