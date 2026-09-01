// TaskDetailPage is the /task/:taskId route for the live agent output stream.

import { Show } from "solid-js";
import { useParams } from "@solidjs/router";

import TaskDetail from "../components/TaskDetail";
import { useAppState } from "../AppState";
import { taskIdFromParam } from "../taskPath";
import { DetailPane } from "../components/Layout";

export default function TaskDetailPage() {
  const s = useAppState();
  const params = useParams();
  const id = () => taskIdFromParam(params.taskId);

  return (
    <Show when={id()} keyed>
      {(taskId) => {
        const t = () => s.taskById(taskId);
        return (
          <DetailPane>
            <TaskDetail
              taskId={taskId}
              taskState={t()?.state ?? "pending"}
              autoFocusPrompt={s.claimInitialTaskFocus(taskId)}
              title={t()?.title}
              error={t()?.error}
              initialPrompt={t()?.initialPrompt}
              inPlanMode={t()?.inPlanMode}
              planContent={t()?.planContent}
              repo={t()?.repos?.[0]?.name ?? ""}
              remoteURL={t()?.repos?.[0]?.remoteURL}
              forge={t()?.repos?.[0]?.forge}
              branch={t()?.repos?.[0]?.branch ?? ""}
              baseBranch={t()?.repos?.[0]?.baseBranch ?? "main"}
              forgeOwner={t()?.forgeOwner}
              forgeRepo={t()?.forgeRepo}
              forgePR={t()?.forgePR}
              ciStatus={t()?.ciStatus}
              ciChecks={t()?.ciChecks}
              harness={t()?.harness ?? ""}
              model={t()?.model}
              diffStat={t()?.diffStat}
              vncPort={t()?.runtime.vncPort ?? 0}
              sudoPassword={t()?.runtime.sudoPassword}
              supportsImages={s.harnesses().find((h) => h.name === (t()?.harness ?? ""))?.supportsImages}
              supportsCompact={s.harnesses().find((h) => h.name === (t()?.harness ?? ""))?.supportsCompact}
              onStop={s.handleStop}
              onPurge={s.handlePurge}
              onRevive={s.handleRevive}
              onFork={s.handleFork}
              onClose={() => s.navigate("/")}
              inputDraft={s.inputDraft(taskId)}
              onInputDraft={(v) => s.setInputDraft(taskId, v)}
              inputImages={s.inputImages(taskId)}
              onInputImages={(imgs) => s.setInputImages(taskId, imgs)}
              onError={s.showWarning}
            />
          </DetailPane>
        );
      }}
    </Show>
  );
}
