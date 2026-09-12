// Go Mode service-item parsing builds the browser-owned voice baseline from authoritative host facts.

const GO_MODE_ITEMS_RESOURCE_URI = "gomode://items";
// Keep these setup-context limits and formatting aligned with Android's ServiceResources.kt.
const MAX_INITIAL_SERVICE_CONTEXT_CHARS = 4000;
const MAX_INITIAL_SERVICE_ITEMS = 20;

interface ServiceItem {
  reference: string | null;
  title: string;
  state: string;
  needsAttention: boolean;
}

interface ServiceItems {
  items: ServiceItem[];
  moreItemsHint: string | null;
  omittedCount: number;
}

export { GO_MODE_ITEMS_RESOURCE_URI };

/** Parse and bound a host-neutral Go Mode item resource for session setup. */
export function initialServiceContext(text: string | null): string {
  if (text === null) return "No visible service items.";
  let source: ServiceItems;
  try {
    source = parseServiceItems(text);
  } catch {
    // Service context is advisory: malformed or version-skewed resource data
    // must not prevent the otherwise independent voice transport from starting.
    return "No visible service items.";
  }
  const limit = Math.min(source.items.length, MAX_INITIAL_SERVICE_ITEMS);
  let included = 0;
  for (let count = 1; count <= limit; count++) {
    const candidate = formatServiceItems(source, count);
    if (candidate.length > MAX_INITIAL_SERVICE_CONTEXT_CHARS) break;
    included = count;
  }
  return formatServiceItems(source, included);
}

function parseServiceItems(text: string): ServiceItems {
  const value: unknown = JSON.parse(text);
  if (!isObject(value) || !Array.isArray(value.items)) {
    throw new Error("gomode://items must be an object with an items array");
  }
  const omittedCount = value.omittedCount === undefined ? 0 : value.omittedCount;
  if (typeof omittedCount !== "number" || !Number.isSafeInteger(omittedCount) || omittedCount < 0) {
    throw new Error("gomode://items omittedCount must be a non-negative integer");
  }
  const moreItemsHint = value.moreItemsHint;
  if (moreItemsHint !== undefined && typeof moreItemsHint !== "string") {
    throw new Error("gomode://items moreItemsHint must be a string");
  }
  return {
    items: value.items.map(parseServiceItem),
    moreItemsHint: moreItemsHint?.slice(0, 512) ?? null,
    omittedCount,
  };
}

function parseServiceItem(value: unknown, index: number): ServiceItem {
  if (!isObject(value)) {
    throw new Error(`gomode://items item ${index} must be an object`);
  }
  if (typeof value.title !== "string" || value.title.trim() === "") {
    throw new Error(`gomode://items item ${index} must have a title`);
  }
  if (value.reference !== undefined && typeof value.reference !== "string") {
    throw new Error(`gomode://items item ${index} reference must be a string`);
  }
  if (value.state !== undefined && typeof value.state !== "string") {
    throw new Error(`gomode://items item ${index} state must be a string`);
  }
  if (value.needsAttention !== undefined && typeof value.needsAttention !== "boolean") {
    throw new Error(`gomode://items item ${index} needsAttention must be a boolean`);
  }
  return {
    reference: value.reference ?? null,
    title: value.title,
    state: value.state ?? "",
    needsAttention: value.needsAttention ?? false,
  };
}

function formatServiceItems(source: ServiceItems, count: number): string {
  if (source.items.length === 0 && source.omittedCount === 0) {
    return "No visible service items.";
  }
  const lines = source.items.slice(0, count).map((item) => {
    const reference = item.reference === null || item.reference === "" ? "" : `${item.reference}: `;
    const state = item.state === "" ? "" : ` (${item.state}${item.needsAttention ? ", needs attention" : ""})`;
    return `- ${reference}${item.title}${state}`;
  });
  const omitted = source.omittedCount + source.items.length - count;
  if (omitted > 0) {
    const hint = source.moreItemsHint === null || source.moreItemsHint === "" ? "" : ` ${source.moreItemsHint}`;
    lines.push(`- … ${omitted} more items omitted.${hint}`);
  }
  return `Current service items:\n${lines.join("\n")}`;
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
