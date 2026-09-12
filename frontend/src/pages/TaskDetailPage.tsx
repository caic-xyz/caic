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
              startedAt={t()?.startedAt}
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
			  model={t()?.reportedModel || t()?.requestedModel}
              costUSD={t()?.costUSD}
              cumulativeInputTokens={t()?.cumulativeInputTokens}
              cumulativeOutputTokens={t()?.cumulativeOutputTokens}
              cumulativeCacheCreationInputTokens={t()?.cumulativeCacheCreationInputTokens}
              cumulativeCacheReadInputTokens={t()?.cumulativeCacheReadInputTokens}
              diffStat={t()?.diffStat}
              vncPort={t()?.runtime.vncPort ?? 0}
              sudoPassword={t()?.runtime.sudoPassword}
              supportsImages={s.harnesses().find((h) => h.name === (t()?.harness ?? ""))?.supportsImages}
              supportsCompact={s.harnesses().find((h) => h.name === (t()?.harness ?? ""))?.supportsCompact}
              rateLimit={t()?.rateLimit}
              now={s.now()}
              onStop={s.handleStop}
              onPurge={s.handlePurge}
              onRevive={s.handleRevive}
              onFork={s.handleFork}
              onQuotaRecovery={s.handleQuotaRecovery}
              parentTaskID={t()?.parentTaskID}
              childTasks={s.tasks()
                .filter((candidate) => candidate.parentTaskID === taskId)
                .map((candidate) => ({ id: candidate.id, title: candidate.title }))}
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
