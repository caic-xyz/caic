// Camera capture dialog: opens webcam, lets user take a photo, returns base64 ImageData.
import { createSignal, onCleanup, onMount, Show } from "solid-js";
import type { ImageData as APIImageData } from "@sdk/types.gen";
import SwitchCameraIcon from "@material-symbols/svg-400/outlined/cameraswitch.svg?solid";
import styles from "./CameraCapture.module.css";

interface Props {
  onCapture: (img: APIImageData) => void;
  onClose: () => void;
}

export default function CameraCapture(props: Props) {
  let videoRef!: HTMLVideoElement;
  let canvasRef!: HTMLCanvasElement;
  const [stream, setStream] = createSignal<MediaStream | null>(null);
  const [error, setError] = createSignal("");
  const [facingMode, setFacingMode] = createSignal<"environment" | "user">("environment");
  const [hasMultiple, setHasMultiple] = createSignal(false);

  async function startCamera(facing: "environment" | "user") {
    // Stop any existing stream before switching.
    stream()?.getTracks().forEach((t) => t.stop());
    try {
      const s = await navigator.mediaDevices.getUserMedia({
        video: { facingMode: facing },
      });
      setStream(s);
      videoRef.srcObject = s;
      await videoRef.play();
      setError("");
    } catch {
      setError("Could not access camera. Check browser permissions.");
    }
  }

  onMount(async () => {
    await startCamera(facingMode());
    // Detect whether multiple cameras are available.
    try {
      const devices = await navigator.mediaDevices.enumerateDevices();
      setHasMultiple(devices.filter((d) => d.kind === "videoinput").length > 1);
    } catch {
      // enumerateDevices not supported — hide switch button.
    }
  });

  onCleanup(() => {
    stream()?.getTracks().forEach((t) => t.stop());
  });

  function switchCamera() {
    const next = facingMode() === "environment" ? "user" : "environment";
    setFacingMode(next);
    void startCamera(next);
  }

  function capture() {
    const w = videoRef.videoWidth;
    const h = videoRef.videoHeight;
    canvasRef.width = w;
    canvasRef.height = h;
    const ctx = canvasRef.getContext("2d");
    if (!ctx) return;
    ctx.drawImage(videoRef, 0, 0, w, h);

    // Convert to JPEG base64.
    const dataURL = canvasRef.toDataURL("image/jpeg", 0.9);
    const base64 = dataURL.split(",")[1];
    props.onCapture({ mediaType: "image/jpeg", data: base64 });
    props.onClose();
  }

  return (
    // eslint-disable-next-line jsx-a11y/no-static-element-interactions, jsx-a11y/click-events-have-key-events -- backdrop dismiss is supplementary to Cancel button and Escape key
    <div class={styles.overlay} onClick={(e) => { if (e.target === e.currentTarget) props.onClose(); }}>
      {/* eslint-disable-next-line jsx-a11y/no-noninteractive-element-interactions -- Escape key dismissal is standard for modal dialogs */}
      <div class={styles.dialog} role="dialog" aria-modal="true" onKeyDown={(e) => { if (e.key === "Escape") props.onClose(); }}>
        {error() ? (
          <p class={styles.error}>{error()}</p>
        ) : (
          <>
            <video ref={(el) => { videoRef = el; }} class={styles.video} autoplay playsinline muted />
            <canvas ref={(el) => { canvasRef = el; }} class={styles.canvas} />
          </>
        )}
        <div class={styles.actions}>
          <Show when={hasMultiple() && !error()}>
            <button class={styles.switchBtn} onClick={switchCamera} title="Switch camera">
              <SwitchCameraIcon width="1.4em" height="1.4em" />
            </button>
          </Show>
          {!error() && (
            <button class={styles.captureBtn} onClick={capture} title="Take photo" />
          )}
          <button class={styles.closeBtn} onClick={() => props.onClose()}>Cancel</button>
        </div>
      </div>
    </div>
  );
}
