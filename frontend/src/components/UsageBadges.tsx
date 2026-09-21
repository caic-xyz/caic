// Usage badges: per-provider grouped pills with color-coded thresholds, an icon
// tooltip that always names the provider, and a backend-reported pricing-phase
// icon tint (e.g. DeepSeek peak hours).

import { Show, For, Switch, Match } from "solid-js";
import type { Accessor } from "solid-js";

import type { ProviderQuota, QuotaRateLimit, QuotaBalance, UsageResp } from "@sdk/types.gen";

import Tooltip from "./Tooltip";
import { currencySign, formatBalance } from "../formatting";
import styles from "./UsageBadges.module.css";

function pctColor(pct: number) {
  if (pct >= 90) return styles.red;
  if (pct >= 80) return styles.yellow;
  return styles.green;
}

function formatReset(iso: string | undefined, now: number): string | undefined {
  if (!iso) return undefined;
  const d = new Date(iso);
  const diffMs = d.getTime() - now;
  if (diffMs <= 0) return "now";
  const hours = Math.floor(diffMs / 3_600_000);
  const mins = Math.floor((diffMs % 3_600_000) / 60_000);
  if (hours >= 24) {
    const days = Math.floor(hours / 24);
    return `in ${days}d ${hours % 24}h`;
  }
  if (hours > 0) return `in ${hours}h ${mins}m`;
  return `in ${mins}m`;
}

function balanceClass(bal: QuotaBalance): string {
  return `${styles.badge} ${bal.total <= 0 ? styles.red : styles.green}`;
}

// hasSpend reports whether the balance carries pay-as-you-go spend info
// (Anthropic-style extra credits or a spend cap).
function hasSpend(bal: QuotaBalance): boolean {
  return (bal.usedCredits ?? 0) !== 0 || (bal.monthlyLimit ?? 0) !== 0;
}

function spendClass(bal: QuotaBalance): string {
  if (!bal.extraEnabled) return `${styles.badge} ${styles.disabled}`;
  return `${styles.badge} ${pctColor(bal.usedPct ?? 0)}`;
}

function spendLabel(bal: QuotaBalance): string {
  const s = currencySign(bal.currency);
  return `${s}${(bal.usedCredits ?? 0).toFixed(0)}/${s}${(bal.monthlyLimit ?? 0).toFixed(0)}`;
}

function spendTooltip(bal: QuotaBalance): string {
  const s = currencySign(bal.currency);
  if (bal.extraEnabled) {
    return `${s}${(bal.usedCredits ?? 0).toFixed(2)} / ${s}${(bal.monthlyLimit ?? 0).toFixed(2)}`;
  }
  return `Disabled — ${s}${(bal.usedCredits ?? 0).toFixed(2)} / ${s}${(bal.monthlyLimit ?? 0).toFixed(2)}`;
}

function RateLimitBadge(props: { rl: QuotaRateLimit; now: Accessor<number>; label: string }) {
  const tip = () => {
    const reset = formatReset(props.rl.resetsAt, props.now());
    return reset ? `${props.label} ${props.rl.window}: ${Math.round(props.rl.usedPct)}% — Resets ${reset}` : undefined;
  };
  return (
    <Tooltip text={tip()}>
      <span class={`${styles.badge} ${pctColor(props.rl.usedPct)}`} data-testid="usage-badge">
        {props.rl.window} {Math.round(props.rl.usedPct)}%
      </span>
    </Tooltip>
  );
}

/** Pricing phase reported by the backend for time-dependent provider pricing. */
type PricingPhase = "peak" | "peak-soon" | "off-peak";

// pricingOf reads the provider's pricing phase from the usage snapshot.
function pricingOf(pq: ProviderQuota): { phase: PricingPhase; transitionAt: number | null } | null {
  if (pq.pricingPhase !== "peak" && pq.pricingPhase !== "peak-soon" && pq.pricingPhase !== "off-peak") {
    return null;
  }
  return {
    phase: pq.pricingPhase,
    transitionAt: pq.pricingTransitionAt ? new Date(pq.pricingTransitionAt).getTime() : null,
  };
}

function pricingClass(pricing: { phase: PricingPhase } | null): string | undefined {
  switch (pricing?.phase) {
    case "peak":
      return styles.pricingPeak;
    case "peak-soon":
      return styles.pricingPeakSoon;
    case "off-peak":
    case undefined:
      return undefined;
  }
}

function formatUTCClock(ts: number): string {
  return `${new Date(ts).toISOString().slice(11, 16)} UTC`;
}

function pricingTooltip(
  label: string,
  pricing: { phase: PricingPhase; transitionAt: number | null },
  now: number,
): string {
  switch (pricing.phase) {
    case "peak":
      return `${label} peak pricing until ${formatUTCClock(pricing.transitionAt ?? now)}`;
    case "peak-soon": {
      const minutes = Math.max(1, Math.round(((pricing.transitionAt ?? now) - now) / 60_000));
      return `${label} peak pricing in ${minutes}m (${formatUTCClock(pricing.transitionAt ?? now)})`;
    }
    case "off-peak":
      return `${label} off-peak pricing`;
  }
}

function pricingAnnouncement(pricing: { phase: PricingPhase }): string {
  switch (pricing.phase) {
    case "peak":
      return "peak pricing";
    case "peak-soon":
      return "peak pricing soon";
    case "off-peak":
      return "off-peak pricing";
  }
}

function ProviderIcon(props: {
  logoUrl?: string;
  label: string;
  pricing: { phase: PricingPhase; transitionAt: number | null } | null;
  now: Accessor<number>;
}) {
  // Name the provider even when there is no money or rate-limit data to show;
  // the pricing tooltip supersedes it for time-dependent pricing.
  const tip = () => (props.pricing ? pricingTooltip(props.label, props.pricing, props.now()) : props.label);
  const iconClass = () => {
    const phase = pricingClass(props.pricing);
    return phase ? `${styles.providerIcon} ${phase}` : styles.providerIcon;
  };
  return (
    <Show when={props.logoUrl} fallback={<span class={styles.providerLabel}>{props.label}</span>}>
      {(url) => (
        <Tooltip text={tip()}>
          <span class={iconClass()} data-testid="provider-pricing-icon" data-pricing-phase={props.pricing?.phase}>
            <img class={styles.providerLogo} src={url()} alt={props.label} />
            <Show when={props.pricing}>
              {(pricing) => <span class={styles.visuallyHidden}>{pricingAnnouncement(pricing())}</span>}
            </Show>
          </span>
        </Tooltip>
      )}
    </Show>
  );
}

function ProviderPill(props: { pq: ProviderQuota; now: Accessor<number> }) {
  const pricing = () => pricingOf(props.pq);

  const badgeSpan = (
    <span class={styles.providerBadges}>
      <For each={props.pq.rateLimits ?? []}>
        {(rl) => <RateLimitBadge rl={rl} now={props.now} label={props.pq.label} />}
      </For>
      <Show when={props.pq.balance}>
        {(bal) => (
          <Show when={bal().total !== 0 || !hasSpend(bal())}>
            <Tooltip text={`${props.pq.label}: ${formatBalance(bal().currency, bal().total)}`}>
              <span class={balanceClass(bal())} data-testid="usage-badge">
                {formatBalance(bal().currency, bal().total)}
              </span>
            </Tooltip>
          </Show>
        )}
      </Show>
      <Show when={props.pq.balance}>
        {(bal) => (
          // Spend-only balances (e.g. Claude) report no wallet total, so the
          // zero balance pill stays hidden and only the spend pill shows.
          <Show when={hasSpend(bal())}>
            <Tooltip text={`${props.pq.label}: ${spendTooltip(bal())}`}>
              <span class={spendClass(bal())} data-testid="usage-badge">
                {spendLabel(bal())}
              </span>
            </Tooltip>
          </Show>
        )}
      </Show>
    </span>
  );

  const content = (
    <>
      <ProviderIcon logoUrl={props.pq.logoUrl} label={props.pq.label} pricing={pricing()} now={props.now} />
      {badgeSpan}
    </>
  );

  return (
    <Switch>
      <Match when={!!props.pq.usageUrl}>
        <a
          class={styles.providerPill}
          data-testid="provider-usage"
          href={props.pq.usageUrl}
          target="_blank"
          rel="noopener noreferrer"
        >
          {content}
        </a>
      </Match>
      <Match when={!props.pq.usageUrl}>
        <span class={styles.providerPill} data-testid="provider-usage">
          {content}
        </span>
      </Match>
    </Switch>
  );
}

export default function UsageBadges(props: { usage: Accessor<UsageResp | null>; now: Accessor<number> }) {
  return (
    <span class={styles.usageRow}>
      <Show when={props.usage()} keyed>
        {(u) => <For each={u.providers}>{(pq) => <ProviderPill pq={pq} now={props.now} />}</For>}
      </Show>
    </span>
  );
}
