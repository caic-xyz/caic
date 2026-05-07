/** Minimal type declaration for @novnc/novnc (noVNC). */
declare module "@novnc/novnc" {
  class RFB {
    constructor(canvas: HTMLElement, url: string, options?: { shared?: boolean });
    scaleViewport: boolean;
    resizeSession: boolean;
    disconnect(): void;
    addEventListener(event: string, handler: (e: Event) => void): void;
    removeEventListener(event: string, handler: (e: Event) => void): void;
  }
  export default RFB;
}
declare module "@novnc/novnc/core/rfb" {
  export { default } from "@novnc/novnc";
}
