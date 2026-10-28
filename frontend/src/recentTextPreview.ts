// Keeps a small root-text excerpt while the complete task conversation is loading.
import type { EventMessage } from "@sdk/types.gen";
import { isChildEvent } from "./grouping";

export interface PreviewText {
  prompt: string;
  response: string;
}

const previewLimit = 16_000;

export class RecentTextPreview {
  value: PreviewText | null = null;

  append(event: EventMessage) {
    if (isChildEvent(event)) return;
    let prompt = this.value?.prompt ?? "";
    let response = this.value?.response ?? "";
    if (event.userInput) {
      prompt = event.userInput.text;
      response = "";
    } else if (event.text) response = event.text.text.slice(-previewLimit);
    else if (event.textDelta) response = (response + event.textDelta.text).slice(-previewLimit);
    else if (event.result?.result) response = event.result.result.slice(-previewLimit);
    else return;
    if (prompt !== this.value?.prompt || response !== this.value?.response)
      this.value = prompt || response ? { prompt, response } : null;
  }
}
