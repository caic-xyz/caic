/** Minimal type declaration for @novnc/novnc (noVNC). */
declare module "@novnc/novnc" {
  class RFB {
    constructor(canvas: HTMLElement, url: string);
    scaleViewport: boolean;
    resizeSession: boolean;
    disconnect(): void;
  }
  export default RFB;
}
declare module "@novnc/novnc/core/rfb" {
  export { default } from "@novnc/novnc";
}
