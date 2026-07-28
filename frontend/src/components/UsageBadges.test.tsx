// Tests for UsageBadges component.

import { describe, it, expect } from "vitest";
import { render } from "@solidjs/testing-library";
import { createSignal } from "solid-js";

import { QuotaProviderAnthropic, QuotaProviderDeepSeek, type UsageResp, type ProviderQuota, type ISOTimestamp } from "@sdk/types.gen";

import UsageBadges from "./UsageBadges";
import styles from "./UsageBadges.module.css";

const [now] = createSignal(Date.now());

function makeRateLimit(window: string, usedPct: number, resetsAt?: ISOTimestamp) {
  return { window, usedPct, resetsAt };
}

function makeBalance(total: number, currency = "USD", granted?: number, toppedUp?: number) {
  return { currency, total, granted, toppedUp };
}

function makeExtra(isEnabled: boolean, usedCredits: number, monthlyLimit: number, usedPct: number, currency = "USD") {
  return { currency, isEnabled, usedCredits, monthlyLimit, usedPct };
}

function makeProvider(overrides: Partial<ProviderQuota> = {}): ProviderQuota {
  return {
    provider: QuotaProviderAnthropic,
    label: "Test",
    logoUrl: "",
    authKind: "apikey",
    usageUrl: "",
    ...overrides,
  };
}

function makeUsage(providers: ProviderQuota[]): UsageResp {
  return { providers, local: { windows: [] } };
}

/** Returns the first badge span (class includes the hashed "badge" module class). */
function getBadge(container: HTMLElement): Element | null {
  return container.querySelector(`.${styles.badge}`);
}

describe("UsageBadges", () => {
  it("renders nothing when usage is null", () => {
    const [usage] = createSignal<UsageResp | null>(null);
    const { container } = render(() => <UsageBadges usage={usage} now={now} />);
    expect(container.querySelector(`.${styles.usageRow}`)?.children.length).toBe(0);
  });

  it("renders a pill per provider", () => {
    const u = makeUsage([
      makeProvider({ provider: QuotaProviderAnthropic, label: "Anthropic", rateLimits: [makeRateLimit("5h", 45)] }),
      makeProvider({ provider: QuotaProviderDeepSeek, label: "DeepSeek", balance: makeBalance(110, "CNY") }),
    ]);
    const [usage] = createSignal<UsageResp>(u);
    const { container } = render(() => <UsageBadges usage={usage} now={now} />);
    expect(container.querySelectorAll(`.${styles.providerPill}`).length).toBe(2);
  });

  describe("rate limit badges", () => {
    it("green when < 80%", () => {
      const u = makeUsage([makeProvider({ rateLimits: [makeRateLimit("5h", 45)] })]);
      const [usage] = createSignal(u);
      const { container } = render(() => <UsageBadges usage={usage} now={now} />);
      const badge = getBadge(container);
      expect(badge?.className).toContain(styles.green);
      expect(badge?.className).not.toContain(styles.yellow);
      expect(badge?.className).not.toContain(styles.red);
    });

    it("yellow when >= 80%", () => {
      const u = makeUsage([makeProvider({ rateLimits: [makeRateLimit("5h", 85)] })]);
      const [usage] = createSignal(u);
      const { container } = render(() => <UsageBadges usage={usage} now={now} />);
      expect(getBadge(container)?.className).toContain(styles.yellow);
    });

    it("red when >= 90%", () => {
      const u = makeUsage([makeProvider({ rateLimits: [makeRateLimit("5h", 95)] })]);
      const [usage] = createSignal(u);
      const { container } = render(() => <UsageBadges usage={usage} now={now} />);
      expect(getBadge(container)?.className).toContain(styles.red);
    });

    it("shows window label and percentage", () => {
      const u = makeUsage([makeProvider({ rateLimits: [makeRateLimit("7d", 12)] })]);
      const [usage] = createSignal(u);
      const { container } = render(() => <UsageBadges usage={usage} now={now} />);
      expect(container.textContent).toContain("7d");
      expect(container.textContent).toContain("12%");
    });
  });

  describe("balance badges", () => {
    it("shows USD balance with $", () => {
      const u = makeUsage([makeProvider({ balance: makeBalance(25.50, "USD") })]);
      const [usage] = createSignal(u);
      const { container } = render(() => <UsageBadges usage={usage} now={now} />);
      expect(container.textContent).toContain("$25.50");
    });

    it("shows CNY balance with ¥", () => {
      const u = makeUsage([makeProvider({ balance: makeBalance(110, "CNY") })]);
      const [usage] = createSignal(u);
      const { container } = render(() => <UsageBadges usage={usage} now={now} />);
      expect(container.textContent).toContain("¥110.00");
    });

    it("shows ?? for unknown currency", () => {
      const u = makeUsage([makeProvider({ balance: makeBalance(100, "EUR") })]);
      const [usage] = createSignal(u);
      const { container } = render(() => <UsageBadges usage={usage} now={now} />);
      expect(container.textContent).toContain("??100.00");
    });

    it("red when balance <= 0", () => {
      const u = makeUsage([makeProvider({ balance: makeBalance(0, "USD") })]);
      const [usage] = createSignal(u);
      const { container } = render(() => <UsageBadges usage={usage} now={now} />);
      expect(getBadge(container)?.className).toContain(styles.red);
    });
  });

  describe("extra usage badges", () => {
    it("shows enabled extra usage", () => {
      const u = makeUsage([makeProvider({ extraUsage: makeExtra(true, 3, 140, 2.1) })]);
      const [usage] = createSignal(u);
      const { container } = render(() => <UsageBadges usage={usage} now={now} />);
      expect(container.textContent).toContain("$3/$140");
    });

    it("shows CNY extra usage with ¥", () => {
      const u = makeUsage([makeProvider({ extraUsage: makeExtra(true, 50, 500, 10, "CNY") })]);
      const [usage] = createSignal(u);
      const { container } = render(() => <UsageBadges usage={usage} now={now} />);
      expect(container.textContent).toContain("¥50/¥500");
    });

    it("shows ?? for unknown currency in extra", () => {
      const u = makeUsage([makeProvider({ extraUsage: makeExtra(true, 10, 100, 10, "EUR") })]);
      const [usage] = createSignal(u);
      const { container } = render(() => <UsageBadges usage={usage} now={now} />);
      expect(container.textContent).toContain("??10/??100");
    });

    it("disabled extra has disabled class", () => {
      const u = makeUsage([makeProvider({ extraUsage: makeExtra(false, 3, 140, 2.1) })]);
      const [usage] = createSignal(u);
      const { container } = render(() => <UsageBadges usage={usage} now={now} />);
      expect(getBadge(container)?.className).toContain(styles.disabled);
    });

    it("hides when usedCredits and monthlyLimit are both 0", () => {
      const u = makeUsage([makeProvider({ extraUsage: makeExtra(true, 0, 0, 0) })]);
      const [usage] = createSignal(u);
      const { container } = render(() => <UsageBadges usage={usage} now={now} />);
      expect(container.textContent).not.toContain("$0/$0");
    });
  });

  describe("provider label", () => {
    it("shows provider label text", () => {
      const u = makeUsage([makeProvider({ label: "DeepSeek" })]);
      const [usage] = createSignal(u);
      const { container } = render(() => <UsageBadges usage={usage} now={now} />);
      expect(container.textContent).toContain("DeepSeek");
    });
  });
});
