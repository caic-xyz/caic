// Material Symbol for each kind of background work, shared by the task card counts and the background strip chips.

import type { Component, JSX } from "solid-js";
import { Dynamic } from "solid-js/web";
import AgentIcon from "@material-symbols/svg-400/outlined/smart_toy.svg?solid";
import CommandIcon from "@material-symbols/svg-400/outlined/terminal.svg?solid";
import WatchIcon from "@material-symbols/svg-400/outlined/visibility.svg?solid";

import type { BackgroundKind } from "../backgroundActivity";

const icons: Record<BackgroundKind, Component<JSX.SvgSVGAttributes<SVGSVGElement>>> = {
  agent: AgentIcon,
  command: CommandIcon,
  watch: WatchIcon,
};

// BackgroundKindIcon is decorative: its owner names the kind.
export default function BackgroundKindIcon(props: { kind: BackgroundKind; class: string }) {
  return <Dynamic component={icons[props.kind]} class={props.class} aria-hidden="true" />;
}
