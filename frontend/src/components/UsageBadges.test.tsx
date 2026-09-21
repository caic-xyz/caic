// Tests for UsageBadges component.

import { describe, it } from "node:test";
import { expect } from "@tests/expect";
import { render } from "@solidjs/testing-library";
import { createSignal } from "solid-js";

import {
  QuotaProviderAnthropic,
  QuotaProviderDeepSeek,
  type UsageResp,
  type ProviderQuota,
  type ISOTimestamp,
  type QuotaBalance,
} from "@sdk/types.gen";

import UsageBadges from "./UsageBadges";
import styles from "./UsageBadges.module.css";

const [now] = createSignal(Date.now());

function makeRateLimit(window: string, usedPct: number, resetsAt?: ISOTimestamp) {
  return { window, usedPct, resetsAt };
}

function makeBalance(total: number, currency = "USD", granted?: number, toppedUp?: number) {
  return { currency, total, granted, toppedUp };
}

/** Balance carrying pay-as-you-go spend info instead of a wallet total. */
function makeSpend(
  extraEnabled: boolean,
  usedCredits: number,
  monthlyLimit: number,
  usedPct: number,
  currency = "USD",
): QuotaBalance {
  return { currency, total: 0, extraEnabled, usedCredits, monthlyLimit, usedPct };
}

function makeProvider(overrides: Partial<ProviderQuota> = {}): ProviderQuota {
  return {
    provider: QuotaProviderAnthropic,
    label: "Test",
    logoUrl: "",
    authKind: "apikey",
    usageUrl: "",
    fetchStatus: "fresh",
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
      makeProvider({
        provider: QuotaProviderAnthropic,
        label: "Anthropic",
        rateLimits: [makeRateLimit("5h", 45)],
      }),
      makeProvider({
        provider: QuotaProviderDeepSeek,
        label: "DeepSeek",
        balance: makeBalance(110, "CNY"),
      }),
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
      const u = makeUsage([makeProvider({ balance: makeBalance(25.5, "USD") })]);
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

  describe("spend badges", () => {
    it("shows enabled spend info", () => {
      const u = makeUsage([makeProvider({ balance: makeSpend(true, 3, 140, 2.1) })]);
      const [usage] = createSignal(u);
      const { container } = render(() => <UsageBadges usage={usage} now={now} />);
      expect(container.textContent).toContain("$3/$140");
    });

    it("shows CNY spend info with ¥", () => {
      const u = makeUsage([makeProvider({ balance: makeSpend(true, 50, 500, 10, "CNY") })]);
      const [usage] = createSignal(u);
      const { container } = render(() => <UsageBadges usage={usage} now={now} />);
      expect(container.textContent).toContain("¥50/¥500");
    });

    it("shows ?? for unknown currency in spend info", () => {
      const u = makeUsage([makeProvider({ balance: makeSpend(true, 10, 100, 10, "EUR") })]);
      const [usage] = createSignal(u);
      const { container } = render(() => <UsageBadges usage={usage} now={now} />);
      expect(container.textContent).toContain("??10/??100");
    });

    it("disabled spend info has disabled class", () => {
      const u = makeUsage([makeProvider({ balance: makeSpend(false, 3, 140, 2.1) })]);
      const [usage] = createSignal(u);
      const { container } = render(() => <UsageBadges usage={usage} now={now} />);
      expect(getBadge(container)?.className).toContain(styles.disabled);
    });

    it("shows nothing when there is no balance", () => {
      // Claude without extra usage reports no balance at all.
      const u = makeUsage([makeProvider({ label: "Claude Code" })]);
      const [usage] = createSignal(u);
      const { container } = render(() => <UsageBadges usage={usage} now={now} />);
      expect(container.textContent).not.toContain("$0/$0");
      expect(container.textContent).not.toContain("$0.00");
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

  describe("provider icon tooltip", () => {
    it("names the provider on hover even without money data", () => {
      const u = makeUsage([
        makeProvider({ provider: QuotaProviderAnthropic, label: "Anthropic", logoUrl: "/logos/anthropic.svg" }),
      ]);
      const [usage] = createSignal(u);
      const { container } = render(() => <UsageBadges usage={usage} now={now} />);
      const wrapper = container.querySelector('[data-testid="provider-pricing-icon"]')?.parentElement;
      expect(wrapper?.hasAttribute("role")).toBe(true);
      wrapper?.dispatchEvent(new MouseEvent("mouseenter"));
      expect(document.body.textContent).toContain("Anthropic");
      wrapper?.dispatchEvent(new MouseEvent("mouseleave"));
    });
  });

  describe("DeepSeek peak pricing indicator", () => {
    function renderAt(ms: number, overrides: Partial<ProviderQuota> = {}) {
      const u = makeUsage([
        makeProvider({
          provider: QuotaProviderDeepSeek,
          label: "DeepSeek",
          logoUrl: "/logos/deepseek.svg",
          balance: makeBalance(110, "CNY"),
          ...overrides,
        }),
      ]);
      const [usage] = createSignal(u);
      return render(() => <UsageBadges usage={usage} now={() => ms} />);
    }

    function hasClass(el: Element | null | undefined, cls: string): boolean {
      return el?.className.split(" ").includes(cls) ?? false;
    }

    function pricingIcon(container: HTMLElement): Element | null {
      return container.querySelector('[data-testid="provider-pricing-icon"]');
    }

    it("tints the icon during peak pricing", () => {
      const { container } = renderAt(Date.parse("2026-09-21T02:00:00Z"));
      const el = pricingIcon(container);
      expect(el?.getAttribute("data-pricing-phase")).toBe("peak");
      expect(hasClass(el, styles.pricingPeak)).toBe(true);
      expect(hasClass(el, styles.pricingPeakSoon)).toBe(false);
      expect(container.textContent).toContain("peak pricing");
    });

    it("tints the icon when peak pricing starts within 30 minutes", () => {
      const { container } = renderAt(Date.parse("2026-09-21T00:45:00Z"));
      const el = pricingIcon(container);
      expect(el?.getAttribute("data-pricing-phase")).toBe("peak-soon");
      expect(hasClass(el, styles.pricingPeakSoon)).toBe(true);
      expect(hasClass(el, styles.pricingPeak)).toBe(false);
      expect(container.textContent).toContain("peak pricing soon");
    });

    it("leaves the icon untinted off-peak", () => {
      const { container } = renderAt(Date.parse("2026-09-19T02:00:00Z"));
      const el = pricingIcon(container);
      expect(el?.getAttribute("data-pricing-phase")).toBe("off-peak");
      expect(hasClass(el, styles.pricingPeak)).toBe(false);
      expect(hasClass(el, styles.pricingPeakSoon)).toBe(false);
      expect(container.textContent).toContain("off-peak pricing");
    });

    it("leaves other providers untinted during DeepSeek peak hours", () => {
      const { container } = renderAt(Date.parse("2026-09-21T02:00:00Z"), {
        provider: QuotaProviderAnthropic,
        label: "Anthropic",
      });
      const el = pricingIcon(container);
      expect(el?.hasAttribute("data-pricing-phase")).toBe(false);
      expect(hasClass(el, styles.pricingPeak)).toBe(false);
      expect(hasClass(el, styles.pricingPeakSoon)).toBe(false);
    });
  });
});
