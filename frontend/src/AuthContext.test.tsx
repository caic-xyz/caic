// Tests native auth access across caic bootstrap, logout, and uncertain sessions.

import { afterEach, beforeEach, it } from "node:test";
import { cleanup, render, waitFor } from "@solidjs/testing-library";
import { expect, vi } from "@tests/expect";

import { APIError } from "@sdk/api.gen";
import type { AuthBootstrapResp } from "@sdk/types.gen";
import { api } from "./api";
import { AuthProvider, useAuth } from "./AuthContext";

let auth: ReturnType<typeof useAuth> | null = null;
const authMessages = vi.fn((message: string) => message);

function currentAuth(): ReturnType<typeof useAuth> {
  if (auth === null) throw new Error("Auth context did not render");
  return auth;
}

function AuthActions() {
  auth = useAuth();
  return null;
}

function renderAuth() {
  render(() => (
    <AuthProvider>
      <AuthActions />
    </AuthProvider>
  ));
  return authMessages;
}

beforeEach(() => {
  window.__CAIC_BOOTSTRAP__ = {
    authProviders: ["github"],
    user: { id: "a", sessionID: "session-a", provider: "github", username: "account-a" },
  };
  authMessages.mockClear();
  window.goModeFrontendDocument = Promise.resolve("auth-document");
  window.gomodeAuth = {
    postMessage(message) {
      const { frontendDocument, frontendRequest, ...authFields } = JSON.parse(message) as Record<string, unknown>;
      expect(frontendDocument).toBe("auth-document");
      expect(typeof frontendRequest).toBe("string");
      authMessages(JSON.stringify(authFields));
    },
  };
  vi.spyOn(api, "logout").mockResolvedValue({ status: "ok" });
  vi.spyOn(api, "getMe").mockResolvedValue({
    id: "a",
    sessionID: "session-a",
    provider: "github",
    username: "account-a",
  });
});

afterEach(() => {
  cleanup();
  auth = null;
  delete window.__CAIC_BOOTSTRAP__;
  delete window.gomodeAuth;
  delete window.goModeFrontendDocument;
  vi.restoreAllMocks();
});

it("grants native access only for signed-in or auth-disabled bootstrap", async () => {
  const postMessage = renderAuth();
  await waitFor(() => expect(postMessage).toHaveBeenCalledWith('{"authChanged":true,"nativeAccess":true}'));
  cleanup();

  window.__CAIC_BOOTSTRAP__ = { authProviders: ["github"], user: null } as unknown as AuthBootstrapResp;
  const loggedOut = renderAuth();
  await waitFor(() => expect(loggedOut).toHaveBeenCalledWith('{"authChanged":true,"nativeAccess":false}'));
  cleanup();

  window.__CAIC_BOOTSTRAP__ = { authProviders: [] };
  const noAuth = renderAuth();
  await waitFor(() => expect(noAuth).toHaveBeenCalledWith('{"authChanged":true,"nativeAccess":true}'));
});

it("refreshes a same-account session in place", async () => {
  const postMessage = renderAuth();
  await waitFor(() => expect(postMessage).toHaveBeenCalledTimes(1));
  const fetch = vi
    .spyOn(globalThis, "fetch")
    .mockResolvedValue(Response.json({ id: "a", sessionID: "session-b", provider: "github", username: "account-a" }));
  await expect(currentAuth().refreshIdentity()).resolves.toBe(true);
  expect(currentAuth().ready()).toBe(true);
  expect(currentAuth().user()?.sessionID).toBe("session-b");
  expect(fetch.mock.calls[0]?.[1]).toMatchObject({ cache: "no-store" });
  expect(postMessage).toHaveBeenNthCalledWith(2, '{"authChanging":true}');
  expect(postMessage).toHaveBeenNthCalledWith(3, '{"authChanged":true,"nativeAccess":true}');
});

it("skips native auth transitions for an unchanged session", async () => {
  const postMessage = renderAuth();
  await waitFor(() => expect(postMessage).toHaveBeenCalledTimes(1));
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    Response.json({ id: "a", sessionID: "session-a", provider: "github", username: "Renamed account" }),
  );

  await expect(currentAuth().refreshIdentity()).resolves.toBe(true);
  expect(currentAuth().ready()).toBe(true);
  expect(currentAuth().user()?.username).toBe("Renamed account");
  expect(postMessage).toHaveBeenCalledTimes(1);
});

it("reloads when the cookie identity changes accounts", async () => {
  const postMessage = renderAuth();
  await waitFor(() => expect(postMessage).toHaveBeenCalledTimes(1));
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    Response.json({ id: "b", sessionID: "session-b", provider: "github", username: "account-b" }),
  );

  await expect(currentAuth().refreshIdentity()).resolves.toBe(false);
  expect(currentAuth().ready()).toBe(false);
  expect(currentAuth().user()?.id).toBe("a");
  expect(postMessage).toHaveBeenNthCalledWith(2, '{"authChanging":true}');
  expect(postMessage).toHaveBeenCalledTimes(2);
});

it("reloads when the refreshed identity is missing", async () => {
  const postMessage = renderAuth();
  await waitFor(() => expect(postMessage).toHaveBeenCalledTimes(1));
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    Response.json({ error: { code: "NOT_FOUND", message: "missing" } }, { status: 404 }),
  );

  await expect(currentAuth().refreshIdentity()).resolves.toBe(false);
  expect(currentAuth().ready()).toBe(false);
  expect(currentAuth().user()?.id).toBe("a");
  expect(postMessage).toHaveBeenNthCalledWith(2, '{"authChanging":true}');
  expect(postMessage).toHaveBeenCalledTimes(2);
});

it("keeps the rendered identity when an identity refresh fails", async () => {
  const postMessage = renderAuth();
  await waitFor(() => expect(postMessage).toHaveBeenCalledTimes(1));
  vi.spyOn(globalThis, "fetch").mockRejectedValue(new Error("network unavailable"));

  await expect(currentAuth().refreshIdentity()).rejects.toThrow("network unavailable");
  expect(currentAuth().ready()).toBe(true);
  expect(currentAuth().user()?.id).toBe("a");
  expect(postMessage).toHaveBeenCalledTimes(1);
});

it("ignores a stale refresh after the identity changes", async () => {
  const postMessage = renderAuth();
  await waitFor(() => expect(postMessage).toHaveBeenCalledTimes(1));
  const probe = Promise.withResolvers<Response>();
  vi.spyOn(globalThis, "fetch").mockReturnValue(probe.promise);
  const refresh = currentAuth().refreshIdentity();
  await Promise.resolve();
  currentAuth().confirmLoggedOut();
  probe.resolve(Response.json({ id: "a", sessionID: "session-a", provider: "github", username: "account-a" }));
  await expect(refresh).resolves.toBe(false);
  expect(currentAuth().user()).toBeNull();
  expect(postMessage).toHaveBeenLastCalledWith('{"authChanged":true,"nativeAccess":false}');
});

it("settles a fallback identity check as logged out when /auth/me returns 404", async () => {
  delete window.__CAIC_BOOTSTRAP__;
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(JSON.stringify({ authProviders: ["github"] }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    }),
  );
  vi.mocked(api.getMe).mockRejectedValue(new APIError(404, "NOT_FOUND"));

  const postMessage = renderAuth();
  await waitFor(() => expect(currentAuth().ready()).toBe(true));
  expect(currentAuth().user()).toBeNull();
  expect(postMessage).toHaveBeenCalledWith('{"authChanged":true,"nativeAccess":false}');
});

it("keeps native monitoring paused until logout settles", async () => {
  const postMessage = renderAuth();
  await waitFor(() => expect(postMessage).toHaveBeenCalledTimes(1));

  const pendingLogout = Promise.withResolvers<{ status: string }>();
  vi.mocked(api.logout).mockReturnValue(pendingLogout.promise);
  const result = currentAuth().logout();
  expect(api.logout).toHaveBeenCalledWith("session-a");
  expect(currentAuth().ready()).toBe(false);
  await Promise.resolve();
  expect(postMessage).toHaveBeenNthCalledWith(2, '{"authChanging":true}');
  expect(postMessage).toHaveBeenCalledTimes(2);

  pendingLogout.resolve({ status: "ok" });
  await result;
  expect(currentAuth().ready()).toBe(true);
  expect(currentAuth().user()).toBeNull();
  expect(postMessage).toHaveBeenNthCalledWith(3, '{"authChanged":true,"nativeAccess":false}');
});

it("refreshes a same-account session after a failed logout", async () => {
  const postMessage = renderAuth();
  await waitFor(() => expect(postMessage).toHaveBeenCalledTimes(1));
  vi.mocked(api.logout).mockRejectedValue(new Error("logout failed"));
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    Response.json({ id: "a", sessionID: "session-b", provider: "github", username: "account-a" }),
  );

  await currentAuth().logout();
  expect(currentAuth().ready()).toBe(true);
  expect(currentAuth().user()?.sessionID).toBe("session-b");
  expect(postMessage).toHaveBeenNthCalledWith(2, '{"authChanging":true}');
  expect(postMessage).toHaveBeenNthCalledWith(3, '{"authChanged":true,"nativeAccess":true}');
});

it("restores readiness after an unchanged session refresh during logout recovery", async () => {
  const postMessage = renderAuth();
  await waitFor(() => expect(postMessage).toHaveBeenCalledTimes(1));
  vi.mocked(api.logout).mockRejectedValue(new Error("logout response lost"));
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    Response.json({ id: "a", sessionID: "session-a", provider: "github", username: "account-a" }),
  );

  await currentAuth().logout();
  expect(currentAuth().ready()).toBe(true);
  expect(currentAuth().user()?.id).toBe("a");
  expect(postMessage).toHaveBeenNthCalledWith(2, '{"authChanging":true}');
  expect(postMessage).toHaveBeenNthCalledWith(3, '{"authChanged":true,"nativeAccess":true}');
});

it("reloads when a failed logout recovers a different account", async () => {
  const postMessage = renderAuth();
  await waitFor(() => expect(postMessage).toHaveBeenCalledTimes(1));
  vi.mocked(api.logout).mockRejectedValue(new Error("logout response lost"));
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    Response.json({ id: "b", sessionID: "session-b", provider: "github", username: "account-b" }),
  );

  await currentAuth().logout();
  expect(currentAuth().ready()).toBe(false);
  expect(currentAuth().user()?.id).toBe("a");
  expect(postMessage).toHaveBeenCalledTimes(2);
  expect(postMessage).toHaveBeenNthCalledWith(2, '{"authChanging":true}');
});

it("reloads when logout recovery confirms 401", async () => {
  const postMessage = renderAuth();
  await waitFor(() => expect(postMessage).toHaveBeenCalledTimes(1));
  vi.mocked(api.logout).mockRejectedValue(new Error("logout response lost"));
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    Response.json({ error: { code: "UNAUTHORIZED", message: "missing" } }, { status: 401 }),
  );

  await currentAuth().logout();
  expect(currentAuth().ready()).toBe(false);
  expect(currentAuth().user()?.id).toBe("a");
  expect(postMessage).toHaveBeenCalledTimes(2);
  expect(postMessage).toHaveBeenNthCalledWith(2, '{"authChanging":true}');
});

it("reloads when logout recovery confirms 404", async () => {
  const postMessage = renderAuth();
  await waitFor(() => expect(postMessage).toHaveBeenCalledTimes(1));
  vi.mocked(api.logout).mockRejectedValue(new Error("logout response lost"));
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    Response.json({ error: { code: "NOT_FOUND", message: "missing" } }, { status: 404 }),
  );

  await currentAuth().logout();
  expect(currentAuth().ready()).toBe(false);
  expect(currentAuth().user()?.id).toBe("a");
  expect(postMessage).toHaveBeenCalledTimes(2);
  expect(postMessage).toHaveBeenNthCalledWith(2, '{"authChanging":true}');
});

it("keeps the rendered identity when logout recovery fails", async () => {
  const postMessage = renderAuth();
  await waitFor(() => expect(postMessage).toHaveBeenCalledTimes(1));
  vi.mocked(api.logout).mockRejectedValue(new Error("logout response lost"));
  vi.spyOn(globalThis, "fetch").mockRejectedValue(new Error("network unavailable"));

  await currentAuth().logout();
  expect(currentAuth().ready()).toBe(true);
  expect(currentAuth().user()?.id).toBe("a");
  expect(postMessage).toHaveBeenCalledTimes(2);
  expect(postMessage).toHaveBeenNthCalledWith(2, '{"authChanging":true}');
});

it("settles logged out after /auth/me confirms the session is missing", async () => {
  const postMessage = renderAuth();
  await waitFor(() => expect(postMessage).toHaveBeenCalledTimes(1));

  currentAuth().confirmLoggedOut();
  await Promise.resolve();
  expect(currentAuth().ready()).toBe(true);
  expect(currentAuth().user()).toBeNull();
  expect(postMessage).toHaveBeenCalledTimes(2);
  expect(postMessage).toHaveBeenNthCalledWith(2, '{"authChanged":true,"nativeAccess":false}');
});

it("cannot publish a late authentication result after its provider is disposed", async () => {
  delete window.__CAIC_BOOTSTRAP__;
  const config = Promise.withResolvers<Response>();
  vi.spyOn(globalThis, "fetch").mockReturnValue(config.promise);
  renderAuth();
  cleanup();
  config.resolve(
    new Response(JSON.stringify({ authProviders: [] }), { headers: { "Content-Type": "application/json" } }),
  );
  await config.promise;
  await Promise.resolve();
  expect(authMessages).not.toHaveBeenCalled();
});

it("keeps pending authentication bound to the page that started it", async () => {
  delete window.__CAIC_BOOTSTRAP__;
  const config = Promise.withResolvers<Response>();
  vi.spyOn(globalThis, "fetch").mockReturnValue(config.promise);
  renderAuth();
  const replacementPost = vi.fn();
  window.goModeFrontendDocument = Promise.resolve("replacement-document");
  window.gomodeAuth = { postMessage: replacementPost };
  config.resolve(
    new Response(JSON.stringify({ authProviders: [] }), { headers: { "Content-Type": "application/json" } }),
  );
  await waitFor(() => expect(currentAuth().ready()).toBe(true));
  await Promise.resolve();
  expect(replacementPost).not.toHaveBeenCalled();
  expect(authMessages).toHaveBeenCalledWith('{"authChanged":true,"nativeAccess":true}');
});
