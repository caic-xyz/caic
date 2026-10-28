// Two browser contexts pair through explicit approval and receive normal caic cookie sessions.

import { test, expect } from "@playwright/test";

const origin = process.env.CAIC_E2E_AUTH_URL ?? "";
if (!origin) throw new Error("Missing configured device-login test origin");

test("device approval, normal permissions, logout, and network protection", async ({ browser }) => {
  const display = await browser.newContext({ baseURL: origin, viewport: { width: 640, height: 480 } });
  const approver = await browser.newContext({ baseURL: origin });
  await display.addInitScript(() => {
    Object.defineProperty(window, "goModeFrontendDocument", { value: Promise.resolve("device-document") });
    (window as typeof window & { gomodeAuth: { postMessage(message: string): void } }).gomodeAuth = {
      postMessage(message: string) {
        const events: string[] = JSON.parse(sessionStorage.getItem("auth-events") ?? "[]");
        const { frontendDocument, frontendRequest, ...auth } = JSON.parse(message);
        if (frontendDocument !== "device-document" || typeof frontendRequest !== "string")
          throw new Error("Invalid bridge owner");
        events.push(JSON.stringify(auth));
        if (auth.authChanged === true && auth.nativeAccess === true) {
          const bridge = window as unknown as { gomodeAuth: { onmessage?: (event: { data: string }) => void } };
          bridge.gomodeAuth.onmessage?.({
            data: JSON.stringify({ frontendDocument, frontendRequest, frontendToken: "device-token" }),
          });
        }
        sessionStorage.setItem("auth-events", JSON.stringify(events));
      },
    };
  });
  const screen = await display.newPage();
  const approval = await approver.newPage();
  const errors: string[] = [];
  screen.on("pageerror", (e) => errors.push(e.message));
  approval.on("pageerror", (e) => errors.push(e.message));
  try {
    await test.step("signed-in approver explicitly approves the displayed code", async () => {
      await screen.goto("/");
      await screen.getByRole("link", { name: "Sign in with another device" }).click();
      const code = await screen.getByLabel("Device code", { exact: true }).innerText();
      expect(code).toMatch(/^[A-Z2-9]{4}-[A-Z2-9]{4}-[A-Z2-9]{4}$/);
      await expect(screen.getByText(`${origin}/device`, { exact: true })).toBeVisible();
      await expect(screen.getByText(/Expires in/)).toBeVisible();
      await approval.goto("/device");
      await expect(approval.getByRole("link", { name: "Sign in with another device" })).toHaveCount(0);
      await approval.getByRole("link", { name: "Sign in with GitLab", exact: true }).click();
      await expect(approval).toHaveURL(`${origin}/device`);
      await expect(approval.getByText("Signed in as device-approver (gitlab).")).toBeVisible();
      await expect(screen.getByRole("link", { name: "Back to caic" })).toHaveCount(0);
      await screen.screenshot({ path: test.info().outputPath("device-display.png"), fullPage: true });
      const input = approval.getByRole("textbox", { name: "Device code" });
      await expect(input).toBeFocused();
      await expect(input).toHaveAttribute("inputmode", "text");
      await expect(approval.getByRole("link", { name: "Back to caic" })).toHaveCount(0);
      let lookups = 0;
      approval.on("request", (request) => {
        if (request.url().endsWith("/auth/device/lookup")) lookups++;
      });
      const raw = code.replaceAll("-", "").toLowerCase();
      await input.pressSequentially(raw.slice(0, -1));
      await input.press("Enter");
      expect(lookups).toBe(0);
      await expect(approval.getByRole("status", { includeHidden: true })).toHaveText("");
      await expect(input).toHaveValue(code.slice(0, -1));
      await approval.screenshot({ path: test.info().outputPath("device-entry.png"), fullPage: true });
      await input.pressSequentially(raw.slice(-1));
      await expect(approval.getByText("127.0.0.0/24", { exact: true })).toBeVisible();
      await expect(approval.getByText(/Never approve a code sent by someone else/)).toBeVisible();
      await expect(approval.getByRole("button", { name: "Approve sign-in" })).toBeVisible();
      expect(lookups).toBe(1);
      await approval.screenshot({ path: test.info().outputPath("device-review.png"), fullPage: true });
      // Lookup alone must leave the display waiting and unauthenticated.
      await expect(screen.getByRole("status")).toHaveText("Waiting for approval…");
      const before = await display.request.get("/auth/me");
      expect(before.status()).toBe(404);
      await approval.getByRole("button", { name: "Approve sign-in" }).click();
      await expect(screen).toHaveURL(`${origin}/`, { timeout: 15_000 });
      expect(
        await screen.evaluate(() => (JSON.parse(sessionStorage.getItem("auth-events") ?? "[]") as string[]).slice(-2)),
      ).toEqual(['{"authChanging":true}', '{"authChanged":true,"nativeAccess":true}']);
      const me = await display.request.get("/auth/me");
      expect(me.status()).toBe(200);
      expect(await me.json()).toMatchObject({
        username: "device-approver",
        provider: "gitlab",
      });
      // Device sign-in reaches the same write, voice and delegation handlers as provider login.
      for (const [method, path, data] of [
        ["POST", "/api/caic/v1/tasks", {}],
        ["GET", "/api/caic/v1/voice/token", undefined],
        ["GET", "/oauth/authorize", undefined],
      ] as const) {
        const deviceResponse = await display.request.fetch(path, { method, data, maxRedirects: 0 });
        const providerResponse = await approver.request.fetch(path, { method, data, maxRedirects: 0 });
        expect(deviceResponse.status()).toBe(providerResponse.status());
      }
      const mcp = await display.request.post("/api/caic/v1/mcp", {
        data: {
          jsonrpc: "2.0",
          id: 1,
          method: "tools/call",
          params: {
            name: "task_create",
            arguments: { prompt: "FAKE_DEVICE_MCP normal device permissions", harness: "claude" },
          },
        },
      });
      const mcpResult = await mcp.json();
      expect(mcpResult.result.isError).not.toBe(true);
      expect(mcpResult.result.structuredContent.taskID).toBeTruthy();
      const created = await display.request.post("/api/caic/v1/tasks", {
        data: { initialPrompt: { text: "FAKE_DEVICE_REST normal device permissions" }, harness: "claude" },
      });
      expect(created.status()).toBe(200);
      expect((await created.json()).id).toBeTruthy();
      const deviceSessions = await (await display.request.get("/auth/sessions")).json();
      const providerSessions = await (await approver.request.get("/auth/sessions")).json();
      const deviceRecord = deviceSessions.sessions.find((item: { current: boolean }) => item.current);
      const providerRecord = providerSessions.sessions.find((item: { current: boolean }) => item.current);
      const deviceLifetime = Date.parse(deviceRecord.expiresAt) - Date.parse(deviceRecord.createdAt);
      const providerLifetime = Date.parse(providerRecord.expiresAt) - Date.parse(providerRecord.createdAt);
      expect(Math.abs(deviceLifetime - providerLifetime)).toBeLessThan(1000);
      expect(Math.abs(deviceLifetime - 30 * 24 * 60 * 60 * 1000)).toBeLessThan(1000);
      const session = (await display.cookies()).find((c) => c.name === "caic_session");
      expect(session).toMatchObject({ httpOnly: true, sameSite: "Lax", path: "/" });
      expect((await display.request.get("/api/caic/v1/server/config")).status()).toBe(200);
      expect((await display.request.post("/auth/logout", { headers: { "X-Caic-CSRF": "1" } })).status()).toBe(200);
      expect((await display.request.get("/auth/me")).status()).toBe(404);
      expect((await approver.request.get("/auth/me")).status()).toBe(200);
      expect(
        (
          await display.request.get("/auth/me", { headers: { Cookie: "", Authorization: `Bearer ${session?.value}` } })
        ).status(),
      ).toBe(404);
    });
    await test.step("approver identifies and revokes the display session without ending its own session", async () => {
      await screen.goto("/device-login");
      const code = await screen.getByLabel("Device code", { exact: true }).innerText();
      await approval.goto("/device");
      await approval.getByRole("textbox", { name: "Device code" }).fill(code);
      await approval.getByRole("button", { name: "Approve sign-in" }).click();
      await expect(screen).toHaveURL(`${origin}/`, { timeout: 15_000 });
      const credential = (await display.cookies()).find((c) => c.name === "caic_session");
      const response = await display.request.get("/auth/sessions");
      expect(response.status()).toBe(200);
      const body: { sessions: { id: string; current: boolean; source: string }[] } = await response.json();
      const device = body.sessions.find((s) => s.current);
      expect(device?.source).toBe("device");
      expect((await display.request.post("/api/caic/v1/tasks", { data: {} })).status()).toBe(400);
      if (!device) throw new Error("Missing current device session");
      const csrf = await approver.request.post(`/auth/sessions/${device.id}/revoke`);
      expect(csrf.status()).toBe(403);
      const wrongOrigin = await approver.request.post(`/auth/sessions/${device.id}/revoke`, {
        headers: { "X-Caic-CSRF": "1", Origin: "https://evil.example" },
      });
      expect(wrongOrigin.status()).toBe(403);
      expect((await display.request.get("/auth/me")).status()).toBe(200);
      await approval.route("**/auth/sessions", (route) =>
        route.fulfill({
          status: 500,
          contentType: "application/json",
          body: JSON.stringify({ error: { code: "INTERNAL_ERROR", message: "injected failure" } }),
        }),
      );
      await approval.goto("/sessions");
      await expect(approval.getByRole("alert")).toHaveText("Could not load sessions.");
      await approval.unroute("**/auth/sessions");
      await approval.getByRole("button", { name: "Reload sessions" }).click();
      await approval.setViewportSize({ width: 320, height: 960 });
      await expect
        .poll(() => approval.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth))
        .toBe(true);
      const row = approval.getByTestId(`session-${device.id}`);
      await expect(row.getByRole("heading", { name: "Device sign-in", exact: true })).toBeVisible();
      await row.getByRole("button", { name: "Sign out session" }).click();
      await expect(row).toHaveCount(0);
      expect((await display.request.get("/auth/me")).status()).toBe(404);
      expect(
        (
          await display.request.get("/auth/me", {
            headers: { Cookie: "", Authorization: `Bearer ${credential?.value}` },
          })
        ).status(),
      ).toBe(404);
      expect((await approver.request.get("/auth/me")).status()).toBe(200);
    });
    await test.step("cross-origin host request is rejected", async () => {
      const response = await display.request.post("/auth/device/start", {
        headers: { Host: "evil.example", "Content-Type": "application/json" },
        data: {},
      });
      expect(response.status()).toBe(403);
    });
    expect(errors).toEqual([]);
  } finally {
    await display.close();
    await approver.close();
  }
});

test("shared pairing views fit narrow screens, wide screens, and native 200% zoom", async ({ browser }, testInfo) => {
  const { mkdir, writeFile } = await import("node:fs/promises");
  const path = await import("node:path");
  const extension = testInfo.outputPath("zoom-extension");
  await mkdir(extension, { recursive: true });
  await writeFile(
    path.join(extension, "manifest.json"),
    JSON.stringify({
      manifest_version: 3,
      name: "Device pairing zoom",
      version: "1.0",
      background: { service_worker: "background.js" },
    }),
  );
  await writeFile(path.join(extension, "background.js"), "chrome.runtime.onInstalled.addListener(() => {});");
  const context = await browser.browserType().launchPersistentContext(testInfo.outputPath("browser-profile"), {
    channel: "chromium",
    headless: true,
    baseURL: origin,
    viewport: { width: 1280, height: 960 },
    args: [`--disable-extensions-except=${extension}`, `--load-extension=${extension}`],
  });
  try {
    const worker = context.serviceWorkers()[0] ?? (await context.waitForEvent("serviceworker"));
    const page = context.pages()[0];
    if (!page) throw new Error("Missing browser page");
    await page.goto("/device");
    await page.getByRole("link", { name: "Sign in with GitLab", exact: true }).click();
    await expect(page.getByRole("textbox", { name: "Device code" })).toBeVisible();
    await page.goto("/device-login");
    const code = await page.getByLabel("Device code", { exact: true }).innerText();
    // The display's code and controls must stay readable without horizontal scrolling.
    for (const width of [320, 1280]) {
      await page.setViewportSize({ width, height: 960 });
      await expect
        .poll(() => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth))
        .toBe(true);
      await expect(page.getByLabel("Device code", { exact: true })).toBeVisible();
      await page.screenshot({ path: testInfo.outputPath(`display-${width}.png`) });
    }
    await worker.evaluate("chrome.tabs.setZoom(2)");
    await expect.poll(() => page.evaluate(() => window.innerWidth)).toBe(640);
    await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await page.screenshot({ path: testInfo.outputPath("display-200-percent.png") });
    await page.goto("/device");
    await page.getByRole("textbox", { name: "Device code" }).fill(code);
    await expect(page.getByRole("button", { name: "Approve sign-in" })).toBeVisible();
    for (const width of [1280, 640]) {
      // Native 200% zoom turns these into 640px and 320px CSS viewports.
      await page.setViewportSize({ width, height: 960 });
      await expect.poll(() => page.evaluate(() => window.innerWidth)).toBe(width / 2);
      await expect
        .poll(() => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth))
        .toBe(true);
      for (const button of await page.getByRole("button").all()) {
        const box = await button.boundingBox();
        if (!box) throw new Error("Missing decision control");
        expect(box.x).toBeGreaterThanOrEqual(0);
        expect(box.x + box.width).toBeLessThanOrEqual(width / 2);
        expect(box.height).toBeGreaterThanOrEqual(44);
      }
      await page.screenshot({ path: testInfo.outputPath(`approval-${width / 2}-200-percent.png`) });
      await page.getByRole("button", { name: "Approve sign-in" }).scrollIntoViewIfNeeded();
      await page.screenshot({ path: testInfo.outputPath(`approval-${width / 2}-200-percent-actions.png`) });
    }
    // Full-page captures at native zoom crop inconsistently in Chromium;
    // use an ordinary 320px viewport for a complete visual reflow record.
    await worker.evaluate("chrome.tabs.setZoom(1)");
    await page.setViewportSize({ width: 320, height: 960 });
    await expect.poll(() => page.evaluate(() => window.innerWidth)).toBe(320);
    await page.screenshot({ path: testInfo.outputPath("approval-320-full.png"), fullPage: true });
  } finally {
    await context.close();
  }
});

test("account changes require re-entering the code before approval", async ({ browser }) => {
  const display = await browser.newContext({ baseURL: origin });
  const approver = await browser.newContext({ baseURL: origin });
  await approver.addInitScript(() => {
    Object.defineProperty(window, "goModeFrontendDocument", { value: Promise.resolve("device-document") });
    Object.defineProperty(window, "gomodeAuth", {
      value: {
        postMessage: (message: string) => {
          const events: unknown[] = JSON.parse(sessionStorage.getItem("test-auth-events") ?? "[]");
          const { frontendDocument, frontendRequest, ...auth } = JSON.parse(message);
          if (frontendDocument !== "device-document" || typeof frontendRequest !== "string")
            throw new Error("Invalid bridge owner");
          events.push(auth);
          if (auth.authChanged === true && auth.nativeAccess === true) {
            const bridge = window as unknown as { gomodeAuth: { onmessage?: (event: { data: string }) => void } };
            bridge.gomodeAuth.onmessage?.({
              data: JSON.stringify({ frontendDocument, frontendRequest, frontendToken: "device-token" }),
            });
          }
          sessionStorage.setItem("test-auth-events", JSON.stringify(events));
        },
      },
    });
  });
  try {
    const screen = await display.newPage();
    const approval = await approver.newPage();
    await screen.goto("/device-login");
    const code = await screen.getByLabel("Device code", { exact: true }).innerText();
    await approval.goto("/device");
    await approval.getByRole("link", { name: "Sign in with GitLab", exact: true }).click();
    await approval.getByRole("textbox", { name: "Device code" }).fill(code);
    await expect(approval.getByRole("button", { name: "Approve sign-in" })).toBeVisible();
    const accountTab = await approver.newPage();
    await accountTab.goto("/device");
    await accountTab.getByRole("button", { name: "Use another account" }).click();
    await expect(accountTab.getByRole("button", { name: "Approve sign-in" })).toHaveCount(0);
    // Obtain the normal signed state cookie, then choose another account at the
    // e2e provider. Redirect-chain requests cannot be intercepted by page.route.
    const start = await approver.request.get("/auth/gitlab/start?next=/device", { maxRedirects: 0 });
    expect(start.status()).toBe(302);
    const providerURL = new URL(start.headers()["location"] ?? "");
    providerURL.searchParams.set("fixture_account", "reviewer");
    await accountTab.goto(providerURL.toString());
    await expect(accountTab.getByText("Signed in as device-reviewer (gitlab).")).toBeVisible();
    // The original tab still renders the previous identity. Its fresh identity
    // check must reject the stale approval before sending a decision.
    await expect(approval.getByText("Signed in as device-approver (gitlab).")).toBeVisible();
    await approval.evaluate(() => sessionStorage.removeItem("test-auth-events"));
    await approval.getByRole("button", { name: "Approve sign-in", exact: true }).click();
    await expect(approval).toHaveURL(`${origin}/device`);
    await expect(approval.getByText("Signed in as device-reviewer (gitlab).")).toBeVisible();
    expect(await approval.evaluate(() => JSON.parse(sessionStorage.getItem("test-auth-events") ?? "[]"))).toEqual([
      { authChanging: true },
      { authChanged: true, nativeAccess: true },
    ]);
    const retainedSession = await approver.request.get("/auth/me");
    expect(retainedSession.status()).toBe(200);
    expect(await retainedSession.json()).toMatchObject({ username: "device-reviewer" });
    await expect(approval.getByRole("textbox", { name: "Device code" })).toHaveValue("");
    await expect(approval.getByRole("button", { name: "Approve sign-in" })).toHaveCount(0);
    await approval.getByRole("textbox", { name: "Device code" }).fill(code);
    await approval.getByRole("button", { name: "Approve sign-in" }).click();
    await expect(screen).toHaveURL(`${origin}/`, { timeout: 15000 });
    const me = await display.request.get("/auth/me");
    expect(await me.json()).toMatchObject({ username: "device-reviewer" });
  } finally {
    await display.close();
    await approver.close();
  }
});

test("pairing errors offer recovery without silently repeating an approval or exchange", async ({ browser }) => {
  const display = await browser.newContext({ baseURL: origin, viewport: { width: 320, height: 960 } });
  const approver = await browser.newContext({ baseURL: origin, viewport: { width: 320, height: 960 } });
  try {
    const screen = await display.newPage();
    const approval = await approver.newPage();
    const errors: string[] = [];
    for (const page of [screen, approval]) page.on("pageerror", (e) => errors.push(e.message));
    await screen.route("**/auth/device/start", (route) =>
      route.fulfill({
        json: {
          device_code: "private-test-secret",
          user_code: "BCDF-GHJK-MNPQ",
          verification_uri: `${origin}/device`,
          expires_in: 600,
          interval: 1,
        },
      }),
    );
    for (const [error, message] of [
      ["expired_token", "This code expired. Create a new code."],
      ["access_denied", "Sign-in denied."],
      ["rate_limited", "Too many attempts. Wait ten minutes before creating a new code."],
      ["audit_failed", "Sign-in could not be recorded. Create a new code."],
      ["session_failed", "A session could not be completed. Create a new code."],
      ["network", "Sign-in could not be confirmed. Check whether this browser is signed in, or create a new code."],
    ]) {
      await test.step(`display ${error}`, async () => {
        await screen.route("**/auth/device/poll", (route) =>
          error === "network"
            ? route.abort()
            : route.fulfill({ status: error === "rate_limited" ? 429 : 400, json: { error } }),
        );
        await screen.goto("/device-login");
        await expect(screen.getByRole("status")).toHaveText(message ?? "");
        await expect(screen.getByRole("button", { name: "Create a new code", exact: true })).toBeVisible();
        if (error === "network")
          await expect(screen.getByRole("button", { name: "Check sign-in", exact: true })).toBeVisible();
        await expect
          .poll(() => screen.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth))
          .toBe(true);
        await screen.unroute("**/auth/device/poll");
      });
    }
    await approval.goto("/device");
    await approval.getByRole("link", { name: "Sign in with GitLab", exact: true }).click();
    const details = {
      user_code: "BCDF-GHJK-MNPQ",
      ip: "192.0.2.0/24",
      user_agent: "Touchscreen car browser",
      created_at: new Date().toISOString(),
      expires_at: new Date(Date.now() + 600000).toISOString(),
      csrf_token: "test-csrf",
    };
    for (const [error, message] of [
      ["expired_token", "This code expired. Create a new code on the device you want to sign in."],
      ["rate_limited", "Too many attempts. Wait 60 seconds before checking the code again."],
      ["audit_failed", "Your approval could not be recorded. Check the code again before approving."],
      ["csrf_failed", "Your account or request changed. Check the code again."],
      ["already_decided", "This code is being processed or already used. Check the other device."],
      ["network", "Your decision could not be confirmed. Check the code again, and check the other device."],
    ]) {
      await test.step(`approval ${error}`, async () => {
        const lookupFailure = error === "expired_token" || error === "rate_limited";
        await approval.route("**/auth/device/lookup", (route) =>
          route.fulfill(
            lookupFailure
              ? { status: error === "rate_limited" ? 429 : 400, headers: { "Retry-After": "60" }, json: { error } }
              : { json: details },
          ),
        );
        await approval.route("**/auth/device/approve", (route) =>
          error === "network" ? route.abort() : route.fulfill({ status: 500, json: { error } }),
        );
        await approval.goto("/device");
        await approval.getByRole("textbox", { name: "Device code" }).fill(details.user_code);
        if (!lookupFailure) await approval.getByRole("button", { name: "Approve sign-in", exact: true }).click();
        await expect(approval.getByRole("status")).toHaveText(message ?? "");
        await expect(approval.getByRole("button", { name: "Approve sign-in", exact: true })).toHaveCount(0);
        await expect
          .poll(() => approval.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth))
          .toBe(true);
        await approval.unroute("**/auth/device/lookup");
        await approval.unroute("**/auth/device/approve");
      });
    }
    expect(errors).toEqual([]);
  } finally {
    await display.close();
    await approver.close();
  }
});

test("same-account session replacement requires code re-entry and rejects an old decision replay", async ({
  browser,
}) => {
  const display = await browser.newContext({ baseURL: origin });
  const approver = await browser.newContext({ baseURL: origin });
  await approver.addInitScript(() => {
    Object.defineProperty(window, "goModeFrontendDocument", { value: Promise.resolve("device-document") });
    Object.defineProperty(window, "gomodeAuth", {
      value: {
        postMessage: (message: string) => {
          const events: unknown[] = JSON.parse(sessionStorage.getItem("test-auth-events") ?? "[]");
          const { frontendDocument, frontendRequest, ...auth } = JSON.parse(message);
          if (frontendDocument !== "device-document" || typeof frontendRequest !== "string")
            throw new Error("Invalid bridge owner");
          events.push(auth);
          if (auth.authChanged === true && auth.nativeAccess === true) {
            const bridge = window as unknown as { gomodeAuth: { onmessage?: (event: { data: string }) => void } };
            bridge.gomodeAuth.onmessage?.({
              data: JSON.stringify({ frontendDocument, frontendRequest, frontendToken: "device-token" }),
            });
          }
          sessionStorage.setItem("test-auth-events", JSON.stringify(events));
        },
      },
    });
  });
  try {
    const screen = await display.newPage();
    const page = await approver.newPage();
    await screen.goto("/device-login");
    const code = await screen.getByLabel("Device code", { exact: true }).innerText();
    await page.goto("/device");
    await page.getByRole("link", { name: "Sign in with GitLab", exact: true }).click();
    await page.getByRole("textbox", { name: "Device code" }).fill(code);
    await expect(page.getByRole("button", { name: "Approve sign-in", exact: true })).toBeVisible();
    const original = (await (await approver.request.get("/auth/me")).json()) as { id: string; sessionID: string };
    // Replace the cookie with a normal provider login without updating this tab.
    expect((await approver.request.get("/auth/gitlab/start?next=/device")).ok()).toBe(true);
    const replacement = (await (await approver.request.get("/auth/me")).json()) as { id: string; sessionID: string };
    expect(replacement.id).toBe(original.id);
    expect(replacement.sessionID).not.toBe(original.sessionID);
    await page.getByRole("button", { name: "Approve sign-in", exact: true }).click();
    await expect(page.getByRole("textbox", { name: "Device code" })).toHaveValue("");
    await expect(page.getByRole("button", { name: "Approve sign-in", exact: true })).toHaveCount(0);
    await page.getByRole("textbox", { name: "Device code" }).fill(code);
    await expect(page.getByRole("button", { name: "Approve sign-in", exact: true })).toBeVisible();
    let staleBody = "";
    let staleHeaders: Record<string, string> = {};
    await page.evaluate(() => sessionStorage.removeItem("test-auth-events"));
    // Replace again after the fresh /me check, just before the actual POST.
    // One-shot interception removes the pattern before the identity reload can abort it.
    let finishReplacement!: () => void;
    const replacementFinished = new Promise<void>((resolve) => {
      finishReplacement = resolve;
    });
    await page.route(
      "**/auth/device/approve",
      async (route) => {
        try {
          staleBody = route.request().postData() ?? "";
          staleHeaders = route.request().headers();
          expect((await approver.request.get("/auth/gitlab/start?next=/device")).ok()).toBe(true);
          await route.continue();
        } finally {
          finishReplacement();
        }
      },
      { times: 1 },
    );
    const rejected = page.waitForResponse((response) => response.url().endsWith("/auth/device/approve"));
    await page.getByRole("button", { name: "Approve sign-in", exact: true }).click();
    expect((await rejected).status()).toBe(401);
    await replacementFinished;
    await expect(page.getByRole("textbox", { name: "Device code" })).toHaveValue("");
    await expect(page.getByText("Signed in as device-approver (gitlab).")).toBeVisible();
    expect(await page.evaluate(() => JSON.parse(sessionStorage.getItem("test-auth-events") ?? "[]"))).toEqual([
      { authChanging: true },
      { authChanged: true, nativeAccess: true },
    ]);
    const current = (await (await approver.request.get("/auth/me")).json()) as { sessionID: string };
    // Removing or updating the public pin cannot reuse the old CSRF proof.
    for (const pin of ["", current.sessionID]) {
      const replay = await approver.request.post("/auth/device/approve", {
        data: JSON.parse(staleBody) as object,
        headers: {
          "X-Device-Login-CSRF": staleHeaders["x-device-login-csrf"] ?? "",
          "X-Caic-Session": pin,
          Origin: origin,
        },
      });
      expect(replay.status()).toBe(403);
    }
    await page.getByRole("textbox", { name: "Device code" }).fill(code);
    await page.getByRole("button", { name: "Approve sign-in", exact: true }).click();
    await expect(screen).toHaveURL(`${origin}/`, { timeout: 15000 });
    expect((await approver.request.get("/auth/me")).ok()).toBe(true);
  } finally {
    await display.close();
    await approver.close();
  }
});

test("a delayed missing-session probe rediscovers the replacement without revoking native access", async ({
  browser,
}) => {
  const display = await browser.newContext({ baseURL: origin });
  const approver = await browser.newContext({ baseURL: origin });
  await approver.addInitScript(() => {
    Object.defineProperty(window, "goModeFrontendDocument", { value: Promise.resolve("device-document") });
    Object.defineProperty(window, "gomodeAuth", {
      value: {
        postMessage: (message: string) => {
          const events: unknown[] = JSON.parse(sessionStorage.getItem("test-auth-events") ?? "[]");
          const { frontendDocument, frontendRequest, ...auth } = JSON.parse(message);
          if (frontendDocument !== "device-document" || typeof frontendRequest !== "string")
            throw new Error("Invalid bridge owner");
          events.push(auth);
          if (auth.authChanged === true && auth.nativeAccess === true) {
            const bridge = window as unknown as { gomodeAuth: { onmessage?: (event: { data: string }) => void } };
            bridge.gomodeAuth.onmessage?.({
              data: JSON.stringify({ frontendDocument, frontendRequest, frontendToken: "device-token" }),
            });
          }
          sessionStorage.setItem("test-auth-events", JSON.stringify(events));
        },
      },
    });
  });
  try {
    const screen = await display.newPage();
    const page = await approver.newPage();
    await screen.goto("/device-login");
    const code = await screen.getByLabel("Device code", { exact: true }).innerText();
    await page.goto("/device");
    await page.getByRole("link", { name: "Sign in with GitLab", exact: true }).click();
    await page.getByRole("textbox", { name: "Device code" }).fill(code);
    await expect(page.getByRole("button", { name: "Approve sign-in", exact: true })).toBeVisible();
    expect(
      (await approver.request.post("/auth/logout", { headers: { "X-Caic-CSRF": "1", Origin: origin } })).ok(),
    ).toBe(true);
    await page.evaluate(() => sessionStorage.removeItem("test-auth-events"));
    await page.route(
      "**/auth/me",
      async (route) => {
        const stale = await route.fetch();
        expect(stale.status()).toBe(404);
        expect((await approver.request.get("/auth/gitlab/start?next=/device")).ok()).toBe(true);
        await route.fulfill({ response: stale });
      },
      { times: 1 },
    );
    await page.getByRole("button", { name: "Approve sign-in", exact: true }).click();
    await expect(page.getByRole("textbox", { name: "Device code" })).toHaveValue("");
    await expect(page.getByText("Signed in as device-approver (gitlab).")).toBeVisible();
    expect(await page.evaluate(() => JSON.parse(sessionStorage.getItem("test-auth-events") ?? "[]"))).toEqual([
      { authChanging: true },
      { authChanged: true, nativeAccess: true },
    ]);
    expect((await approver.request.get("/auth/me")).ok()).toBe(true);
    await page.getByRole("textbox", { name: "Device code" }).fill(code);
    await page.getByRole("button", { name: "Approve sign-in", exact: true }).click();
    await expect(screen).toHaveURL(`${origin}/`, { timeout: 15000 });
  } finally {
    await display.close();
    await approver.close();
  }
});

test("session sign-out rediscovers a replacement cookie before settling native identity", async ({ browser }) => {
  const context = await browser.newContext({ baseURL: origin });
  await context.addInitScript(() => {
    Object.defineProperty(window, "goModeFrontendDocument", { value: Promise.resolve("device-document") });
    Object.defineProperty(window, "gomodeAuth", {
      value: {
        postMessage: (message: string) => {
          const events: unknown[] = JSON.parse(sessionStorage.getItem("test-auth-events") ?? "[]");
          const { frontendDocument, frontendRequest, ...auth } = JSON.parse(message);
          if (frontendDocument !== "device-document" || typeof frontendRequest !== "string")
            throw new Error("Invalid bridge owner");
          events.push(auth);
          if (auth.authChanged === true && auth.nativeAccess === true) {
            const bridge = window as unknown as { gomodeAuth: { onmessage?: (event: { data: string }) => void } };
            bridge.gomodeAuth.onmessage?.({
              data: JSON.stringify({ frontendDocument, frontendRequest, frontendToken: "device-token" }),
            });
          }
          sessionStorage.setItem("test-auth-events", JSON.stringify(events));
        },
      },
    });
  });
  try {
    const page = await context.newPage();
    expect((await context.request.get("/auth/gitlab/start?next=/device")).ok()).toBe(true);
    const before = (await (await context.request.get("/auth/me")).json()) as { sessionID: string };
    await page.goto("/sessions");
    const action = page.getByTestId(`session-${before.sessionID}`).getByRole("button", { name: "Sign out session" });
    await expect(action).toBeVisible();
    await page.evaluate(() => sessionStorage.removeItem("test-auth-events"));
    let replacement = "";
    await page.route(`**/auth/sessions/${before.sessionID}/revoke`, async (route) => {
      const revoked = await route.fetch();
      expect(revoked.ok()).toBe(true);
      expect((await context.request.get("/auth/gitlab/start?next=/device")).ok()).toBe(true);
      const current = (await (await context.request.get("/auth/me")).json()) as { sessionID: string };
      replacement = current.sessionID;
      expect(replacement).not.toBe(before.sessionID);
      await route.fulfill({ response: revoked });
    });
    await action.click();
    await expect.poll(() => replacement).not.toBe("");
    await expect(page.getByTestId(`session-${replacement}`)).toBeVisible();
    expect(await page.evaluate(() => JSON.parse(sessionStorage.getItem("test-auth-events") ?? "[]"))).toEqual([
      { authChanging: true },
      { authChanged: true, nativeAccess: true },
    ]);
    expect(
      (
        await context.request.post(`/auth/sessions/${replacement}/revoke`, {
          headers: { "X-Caic-CSRF": "1", "X-Caic-Session": before.sessionID, Origin: origin },
        })
      ).status(),
    ).toBe(401);
    expect((await context.request.get("/auth/me")).ok()).toBe(true);
  } finally {
    await context.close();
  }
});

test("stale account logout preserves a same-account replacement session", async ({ browser }) => {
  const context = await browser.newContext({ baseURL: origin });
  await context.addInitScript(() => {
    Object.defineProperty(window, "goModeFrontendDocument", { value: Promise.resolve("device-document") });
    (window as typeof window & { gomodeAuth: { postMessage(message: string): void } }).gomodeAuth = {
      postMessage(message: string) {
        const events = JSON.parse(sessionStorage.getItem("logout-auth-events") ?? "[]") as unknown[];
        const { frontendDocument, frontendRequest, ...auth } = JSON.parse(message);
        if (frontendDocument !== "device-document" || typeof frontendRequest !== "string")
          throw new Error("Invalid bridge owner");
        events.push(auth);
        if (auth.authChanged === true && auth.nativeAccess === true) {
          const bridge = window as unknown as { gomodeAuth: { onmessage?: (event: { data: string }) => void } };
          bridge.gomodeAuth.onmessage?.({
            data: JSON.stringify({ frontendDocument, frontendRequest, frontendToken: "device-token" }),
          });
        }
        sessionStorage.setItem("logout-auth-events", JSON.stringify(events));
      },
    };
  });
  try {
    const page = await context.newPage();
    await page.goto("/device");
    await page.getByRole("link", { name: "Sign in with GitLab", exact: true }).click();
    await expect(page.getByRole("button", { name: "Use another account", exact: true })).toBeVisible();
    await expect
      .poll(() => page.evaluate(() => JSON.parse(sessionStorage.getItem("logout-auth-events") ?? "[]")))
      .toContainEqual({ authChanged: true, nativeAccess: true });
    const original = await (await context.request.get("/auth/me")).json();
    const originalCookie = (await context.cookies()).find((cookie) => cookie.name === "caic_session");
    if (!originalCookie) throw new Error("Missing original session");
    expect((await context.request.get("/auth/gitlab/start?next=/device")).ok()).toBe(true);
    const replacement = await (await context.request.get("/auth/me")).json();
    expect(replacement.id).toBe(original.id);
    expect(replacement.sessionID).not.toBe(original.sessionID);
    await page.evaluate(() => sessionStorage.removeItem("logout-auth-events"));
    const rejected = page.waitForResponse((response) => response.url().endsWith("/auth/logout"));
    await page.getByRole("button", { name: "Use another account", exact: true }).click();
    expect((await rejected).status()).toBe(401);
    await expect(page.getByRole("button", { name: "Use another account", exact: true })).toBeVisible();
    expect(await (await context.request.get("/auth/me")).json()).toMatchObject({ sessionID: replacement.sessionID });
    const sessions = await (await context.request.get("/auth/sessions")).json();
    expect(sessions.sessions).toContainEqual(expect.objectContaining({ id: replacement.sessionID, current: true }));
    await expect
      .poll(() => page.evaluate(() => JSON.parse(sessionStorage.getItem("logout-auth-events") ?? "[]")))
      .toEqual([{ authChanging: true }, { authChanged: true, nativeAccess: true }]);
    // A fresh action uses the recovered session and revokes only that session.
    const loggedOut = page.waitForResponse((response) => response.url().endsWith("/auth/logout"));
    await page.getByRole("button", { name: "Use another account", exact: true }).click();
    expect((await loggedOut).status()).toBe(200);
    await expect(page.getByRole("link", { name: "Sign in with GitLab", exact: true })).toBeVisible();
    expect((await context.request.get("/auth/me")).status()).toBe(404);
    const remaining = await context.request.get("/auth/sessions", {
      headers: { Cookie: "", Authorization: `Bearer ${originalCookie.value}` },
    });
    expect(remaining.status()).toBe(200);
    expect((await remaining.json()).sessions).not.toContainEqual(
      expect.objectContaining({ id: replacement.sessionID }),
    );
  } finally {
    await context.close();
  }
});
