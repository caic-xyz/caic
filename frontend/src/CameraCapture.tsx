// Camera capture dialog: opens webcam, lets user take a photo, returns base64 ImageData.
import { createSignal, onCleanup, onMount } from "solid-js";
import type { ImageData as APIImageData } from "@sdk/types.gen";
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

  onMount(async () => {
    try {
      const s = await navigator.mediaDevices.getUserMedia({
        video: { facingMode: "environment" },
      });
      setStream(s);
      videoRef.srcObject = s;
      await videoRef.play();
    } catch {
      setError("Could not access camera. Check browser permissions.");
    }
  });

  onCleanup(() => {
    stream()?.getTracks().forEach((t) => t.stop());
  });

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
    <div class={styles.overlay} onClick={() => props.onClose()}>
      <div class={styles.dialog} onClick={(e) => e.stopPropagation()}>
        {error() ? (
          <p class={styles.error}>{error()}</p>
        ) : (
          <>
            <video ref={(el) => { videoRef = el; }} class={styles.video} autoplay playsinline muted />
            <canvas ref={(el) => { canvasRef = el; }} class={styles.canvas} />
          </>
        )}
        <div class={styles.actions}>
          {!error() && (
            <button class={styles.captureBtn} onClick={capture} title="Take photo" />
          )}
          <button class={styles.closeBtn} onClick={() => props.onClose()}>Cancel</button>
        </div>
      </div>
    </div>
  );
}
