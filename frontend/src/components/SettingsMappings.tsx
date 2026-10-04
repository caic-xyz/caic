// SettingsMappings edits host mounts in container-destination order without moving focused rows.

import { createMemo, createSignal, For, onCleanup, Show } from "solid-js";
import type { CacheMappingResp, MountMappingResp } from "@sdk/types.gen";
import styles from "./SettingsMappings.module.css";

type Mapping = CacheMappingResp & { readOnly?: boolean };

// md's container user lives in /home/user. Resolve tilde notation for sorting
// alongside absolute destinations; server-resolved paths own saved defaults.
export function containerSortPath(path: string): string {
  if (!path) return "";
  const expanded = path === "~" ? "/home/user" : path.startsWith("~/") ? `/home/user/${path.slice(2)}` : path;
  const absolute = expanded.startsWith("/");
  const segments: string[] = [];
  // Match md.ResolveContainerPath's POSIX cleaning for unsaved destinations
  // and built-in cache paths as well as server-resolved saved destinations.
  for (const segment of expanded.split("/")) {
    if (!segment || segment === ".") continue;
    if (segment === "..") {
      if (segments.length > 0 && segments.at(-1) !== "..") segments.pop();
      else if (!absolute) segments.push(segment);
    } else segments.push(segment);
  }
  return `${absolute ? "/" : ""}${segments.join("/")}` || ".";
}

type SettingsMappingsProps = {
  isDirty: (input: HTMLInputElement) => boolean;
} & (
  | {
      mounts: false;
      items: CacheMappingResp[];
      setItems: (items: CacheMappingResp[]) => void;
      save: (items: CacheMappingResp[]) => void;
    }
  | {
      mounts: true;
      items: MountMappingResp[];
      setItems: (items: MountMappingResp[]) => void;
      save: (items: MountMappingResp[]) => void;
    }
);

export default function SettingsMappings(props: SettingsMappingsProps) {
  const [editing, setEditing] = createSignal(false);
  const hostInputs = new Map<number, HTMLInputElement>();
  const write = (items: Mapping[], persist: boolean) => {
    if (props.mounts) {
      const mounts = items.map((item) => ({ ...item, readOnly: item.readOnly ?? false }));
      props.setItems(mounts);
      if (persist) props.save(mounts);
    } else {
      props.setItems(items);
      if (persist) props.save(items);
    }
  };
  const save = () => {
    if (props.mounts) props.save(props.items);
    else props.save(props.items);
  };
  const order = createMemo<number[]>((previous) => {
    // Keep the same DOM order throughout an edit, including server replies.
    if (editing() && previous) return previous;
    return props.items
      .map((item, index) => ({
        index,
        path: containerSortPath(item.resolvedContainerPath || item.containerPath || item.hostPath),
      }))
      .sort((a, b) => {
        if (!a.path) return b.path ? 1 : 0;
        if (!b.path) return -1;
        return a.path < b.path ? -1 : a.path > b.path ? 1 : 0;
      })
      .map(({ index }) => index);
  });
  const update = (index: number, fields: Partial<MountMappingResp>) => {
    write(
      props.items.map((item, i) => (i === index ? { ...item, ...fields, resolvedContainerPath: undefined } : item)),
      false,
    );
  };
  return (
    <div
      class={styles.mappings}
      onFocusOut={(e) => {
        if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setEditing(false);
      }}
    >
      <Show when={props.items.length > 0}>
        <div class={styles.columns} aria-hidden="true">
          <span class={styles.enabledHeading}>Enabled</span>
          <span class={styles.hostHeading}>Host path</span>
          <span class={styles.columnArrow}>→</span>
          <span class={styles.destinationHeading}>Container path</span>
          <Show when={props.mounts}>
            <span class={styles.readOnlyHeading}>Read only</span>
          </Show>
        </div>
      </Show>
      <For each={order()}>
        {(index) => {
          onCleanup(() => hostInputs.delete(index));
          return (
            <Show when={props.items[index]}>
              {(item) => {
                const defaultPath = () => {
                  if (!item().resolvedContainerPath) return "";
                  return item().hostPath.startsWith("~/") || item().hostPath === "~"
                    ? item().hostPath
                    : item().resolvedContainerPath;
                };
                return (
                  <div
                    class={styles.row}
                    data-testid={props.mounts ? "custom-mount-row" : "cache-mapping-row"}
                    data-state={item().enabled ? "enabled" : "disabled"}
                  >
                    <label class={styles.toggle}>
                      <input
                        type="checkbox"
                        aria-label={props.mounts ? "Enable custom mount" : "Enable custom cache"}
                        checked={item().enabled}
                        onChange={(e) => {
                          const items = props.items.map((mapping, i) =>
                            i === index ? { ...mapping, enabled: e.currentTarget.checked } : mapping,
                          );
                          write(items, true);
                        }}
                      />
                      <span class={styles.toggleText}>Enabled</span>
                    </label>
                    <label class={styles.host}>
                      <input
                        type="text"
                        aria-label="Host path"
                        ref={(input) => hostInputs.set(index, input)}
                        value={item().hostPath}
                        placeholder={props.mounts ? "~/Documents" : "~/.cache/tool"}
                        onFocus={() => setEditing(true)}
                        onInput={(e) => {
                          update(index, { hostPath: e.currentTarget.value });
                        }}
                        onBlur={(e) => {
                          if (props.isDirty(e.currentTarget)) save();
                        }}
                      />
                    </label>
                    <span class={styles.arrow} data-testid="mapping-arrow" aria-hidden="true">
                      →
                    </span>
                    <label class={styles.destination}>
                      <input
                        type="text"
                        aria-label="Container path"
                        value={item().containerPath}
                        placeholder={defaultPath() || "Container path"}
                        onFocus={() => setEditing(true)}
                        onInput={(e) => {
                          update(index, { containerPath: e.currentTarget.value });
                        }}
                        onBlur={(e) => {
                          if (props.isDirty(e.currentTarget)) save();
                        }}
                      />
                    </label>
                    <Show when={props.mounts}>
                      <label class={styles.readOnly} data-testid="mount-read-only">
                        <input
                          type="checkbox"
                          aria-label="Read only"
                          checked={(item() as Mapping).readOnly ?? false}
                          onChange={(e) => {
                            const items = props.items.map((mapping, i) =>
                              i === index ? { ...mapping, readOnly: e.currentTarget.checked } : mapping,
                            );
                            write(items, true);
                          }}
                        />
                        <span class={styles.readOnlyText}>Read only</span>
                      </label>
                    </Show>
                    <button
                      type="button"
                      class={styles.remove}
                      aria-label={props.mounts ? "Remove mount" : "Remove cache"}
                      onClick={() => {
                        setEditing(false);
                        const items = props.items.filter((_, i) => i !== index);
                        write(items, true);
                      }}
                    >
                      ×
                    </button>
                  </div>
                );
              }}
            </Show>
          );
        }}
      </For>
      <button
        type="button"
        class={styles.add}
        aria-label={props.mounts ? "Add mount" : "Add cache"}
        title={props.mounts ? "Add mount" : "Add cache"}
        onClick={() => {
          const index = props.items.length;
          setEditing(false);
          write(
            [
              ...props.items,
              { hostPath: "", containerPath: "", enabled: true, ...(props.mounts ? { readOnly: false } : {}) },
            ],
            false,
          );
          queueMicrotask(() => hostInputs.get(index)?.focus());
        }}
      >
        <span aria-hidden="true">+</span>
      </button>
    </div>
  );
}
