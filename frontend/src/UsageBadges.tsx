// Usage badges: per-provider grouped pills with color-coded thresholds.
import { Show, For, Switch, Match } from "solid-js";
import type { Accessor } from "solid-js";
import type { ProviderQuota, QuotaRateLimit, QuotaBalance, QuotaExtraUsage, UsageResp } from "@sdk/types.gen";
import Tooltip from "./Tooltip";
import { currencySign, formatBalance } from "./formatting";
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

function extraClass(extra: QuotaExtraUsage): string {
  if (!extra.isEnabled) return `${styles.badge} ${styles.disabled}`;
  return `${styles.badge} ${pctColor(extra.usedPct)}`;
}

function balanceClass(bal: QuotaBalance): string {
  return `${styles.badge} ${bal.total <= 0 ? styles.red : styles.green}`;
}

function extraLabel(extra: QuotaExtraUsage): string {
  const s = currencySign(extra.currency);
  return `${s}${extra.usedCredits.toFixed(0)}/${s}${extra.monthlyLimit.toFixed(0)}`;
}

function extraTooltip(extra: QuotaExtraUsage): string {
  const s = currencySign(extra.currency);
  if (extra.isEnabled) {
    return `${s}${extra.usedCredits.toFixed(2)} / ${s}${extra.monthlyLimit.toFixed(2)}`;
  }
  return `Disabled — ${s}${extra.usedCredits.toFixed(2)} / ${s}${extra.monthlyLimit.toFixed(2)}`;
}

function RateLimitBadge(props: { rl: QuotaRateLimit; now: Accessor<number>; label: string }) {
  const tip = () => {
    const reset = formatReset(props.rl.resetsAt, props.now());
    return reset ? `${props.label} ${props.rl.window}: ${Math.round(props.rl.usedPct)}% — Resets ${reset}` : undefined;
  };
  return (
    <Tooltip text={tip()}>
      <span class={`${styles.badge} ${pctColor(props.rl.usedPct)}`}>
        {props.rl.window} {Math.round(props.rl.usedPct)}%
      </span>
    </Tooltip>
  );
}

function ProviderIcon(props: { logoUrl?: string; label: string }) {
  return (
    <Show
      when={props.logoUrl}
      fallback={<span class={styles.providerLabel}>{props.label}</span>}
    >
      {(url) => <img class={styles.providerLogo} src={url()} alt={props.label} />}
    </Show>
  );
}

function ProviderPill(props: { pq: ProviderQuota; now: Accessor<number> }) {
  const badgeSpan = (
    <span class={styles.providerBadges}>
      <For each={props.pq.rateLimits ?? []}>
        {(rl) => <RateLimitBadge rl={rl} now={props.now} label={props.pq.label} />}
      </For>
      <Show when={props.pq.balance}>
        {(bal) => (
          <Tooltip text={`${props.pq.label}: ${formatBalance(bal().currency, bal().total)}`}>
            <span class={balanceClass(bal())}>
              {formatBalance(bal().currency, bal().total)}
            </span>
          </Tooltip>
        )}
      </Show>
      <Show when={props.pq.extraUsage}>
        {(extra) => (
          <Show when={extra().usedCredits !== 0 || extra().monthlyLimit !== 0}>
            <Tooltip text={`${props.pq.label}: ${extraTooltip(extra())}`}>
              <span class={extraClass(extra())}>
                {extraLabel(extra())}
              </span>
            </Tooltip>
          </Show>
        )}
      </Show>
    </span>
  );

  const content = (
    <>
      <ProviderIcon logoUrl={props.pq.logoUrl} label={props.pq.label} />
      {badgeSpan}
    </>
  );

  return (
    <Switch>
      <Match when={!!props.pq.usageUrl}>
        <a class={styles.providerPill} href={props.pq.usageUrl} target="_blank" rel="noopener noreferrer">
          {content}
        </a>
      </Match>
      <Match when={!props.pq.usageUrl}>
        <span class={styles.providerPill}>{content}</span>
      </Match>
    </Switch>
  );
}

export default function UsageBadges(props: { usage: Accessor<UsageResp | null>; now: Accessor<number> }) {
  return (
    <span class={styles.usageRow}>
      <Show when={props.usage()} keyed>
        {(u) => (
          <For each={u.providers}>
            {(pq) => <ProviderPill pq={pq} now={props.now} />}
          </For>
        )}
      </Show>
    </span>
  );
}
