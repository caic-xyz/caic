// Tests native auth access across caic bootstrap, logout, and uncertain sessions.

import { afterEach, beforeEach, it } from "node:test";
import { cleanup, render, waitFor } from "@solidjs/testing-library";
import { expect, vi } from "@tests/expect";

import { APIError } from "@sdk/api.gen";
import type { AuthBootstrapResp } from "@sdk/types.gen";
import { api } from "./api";
import { AuthProvider, useAuth } from "./AuthContext";

let auth: ReturnType<typeof useAuth> | null = null;

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
  return window.gomodeAuth?.postMessage;
}

beforeEach(() => {
  window.__CAIC_BOOTSTRAP__ = {
    authProviders: ["github"],
    user: { id: "a", provider: "github", username: "account-a" },
  };
  window.gomodeAuth = { postMessage: vi.fn() };
  vi.spyOn(api, "logout").mockResolvedValue({ status: "ok" });
  vi.spyOn(api, "getMe").mockResolvedValue({ id: "a", provider: "github", username: "account-a" });
});

afterEach(() => {
  cleanup();
  auth = null;
  delete window.__CAIC_BOOTSTRAP__;
  delete window.gomodeAuth;
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
  expect(currentAuth().ready()).toBe(false);
  expect(postMessage).toHaveBeenNthCalledWith(2, '{"authChanging":true}');
  expect(postMessage).toHaveBeenCalledTimes(2);

  pendingLogout.resolve({ status: "ok" });
  await result;
  expect(currentAuth().ready()).toBe(true);
  expect(currentAuth().user()).toBeNull();
  expect(postMessage).toHaveBeenNthCalledWith(3, '{"authChanged":true,"nativeAccess":false}');
});

it("restores a confirmed signed-in user after a failed logout", async () => {
  const postMessage = renderAuth();
  await waitFor(() => expect(postMessage).toHaveBeenCalledTimes(1));
  vi.mocked(api.logout).mockRejectedValue(new Error("logout failed"));
  vi.mocked(api.getMe).mockResolvedValue({ id: "b", provider: "github", username: "account-b" });

  await currentAuth().logout();
  expect(currentAuth().ready()).toBe(true);
  expect(currentAuth().user()?.username).toBe("account-b");
  expect(postMessage).toHaveBeenNthCalledWith(2, '{"authChanging":true}');
  expect(postMessage).toHaveBeenNthCalledWith(3, '{"authChanged":true,"nativeAccess":true}');
});

it("settles logged out when identity check confirms 401 after logout error", async () => {
  const postMessage = renderAuth();
  await waitFor(() => expect(postMessage).toHaveBeenCalledTimes(1));
  vi.mocked(api.logout).mockRejectedValue(new Error("logout response lost"));
  vi.mocked(api.getMe).mockRejectedValue(new APIError(401, "UNAUTHORIZED"));

  await currentAuth().logout();
  expect(currentAuth().ready()).toBe(true);
  expect(currentAuth().user()).toBeNull();
  expect(postMessage).toHaveBeenNthCalledWith(3, '{"authChanged":true,"nativeAccess":false}');
});

it("settles logged out when identity check confirms 404 after logout error", async () => {
  const postMessage = renderAuth();
  await waitFor(() => expect(postMessage).toHaveBeenCalledTimes(1));
  vi.mocked(api.logout).mockRejectedValue(new Error("logout response lost"));
  vi.mocked(api.getMe).mockRejectedValue(new APIError(404, "NOT_FOUND"));

  await currentAuth().logout();
  expect(currentAuth().ready()).toBe(true);
  expect(currentAuth().user()).toBeNull();
  expect(postMessage).toHaveBeenNthCalledWith(3, '{"authChanged":true,"nativeAccess":false}');
});

it("keeps the previous user hidden and native paused when identity is uncertain", async () => {
  const postMessage = renderAuth();
  await waitFor(() => expect(postMessage).toHaveBeenCalledTimes(1));
  vi.mocked(api.logout).mockRejectedValue(new Error("logout response lost"));
  vi.mocked(api.getMe).mockRejectedValue(new Error("network unavailable"));

  await currentAuth().logout();
  expect(currentAuth().ready()).toBe(false);
  expect(currentAuth().user()?.username).toBe("account-a");
  expect(postMessage).toHaveBeenCalledTimes(2);
  expect(postMessage).toHaveBeenNthCalledWith(2, '{"authChanging":true}');
});

it("settles logged out after /auth/me confirms the session is missing", async () => {
  const postMessage = renderAuth();
  await waitFor(() => expect(postMessage).toHaveBeenCalledTimes(1));

  currentAuth().confirmLoggedOut();
  expect(currentAuth().ready()).toBe(true);
  expect(currentAuth().user()).toBeNull();
  expect(postMessage).toHaveBeenCalledTimes(2);
  expect(postMessage).toHaveBeenNthCalledWith(2, '{"authChanged":true,"nativeAccess":false}');
});
