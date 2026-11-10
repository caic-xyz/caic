// Observes browser reachability and route hints for owned connection recovery.
export function observeNetworkChanges(changed: () => void): () => void {
  const connection = (navigator as Navigator & { connection?: EventTarget & { type?: string } }).connection;
  let online = navigator.onLine;
  let type = connection?.type;
  const reachabilityChanged = (): void => {
    online = navigator.onLine;
    type = connection?.type;
    changed();
  };
  const connectionChanged = (): void => {
    // Quality estimates (RTT, downlink, effectiveType) also emit change events.
    // Only reachability or an exposed transport change warrants replacing sockets.
    if (online !== navigator.onLine || type !== connection?.type) reachabilityChanged();
  };
  window.addEventListener("offline", reachabilityChanged);
  window.addEventListener("online", reachabilityChanged);
  connection?.addEventListener("change", connectionChanged);
  return () => {
    window.removeEventListener("offline", reachabilityChanged);
    window.removeEventListener("online", reachabilityChanged);
    connection?.removeEventListener("change", connectionChanged);
  };
}
