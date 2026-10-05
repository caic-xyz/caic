// Encodes current task-log controls and adapts historical schemas to semantic messages.

package agent

import (
	"bytes"
	"encoding/json"
	"fmt"

	v1 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v1"
	v2 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v2"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

func logV1MetaMessageFromData(m *v1.MetaMessage) MetaMessage {
	return MetaMessage{
		MessageType:       m.MessageType,
		Version:           m.Version,
		Prompt:            m.Prompt,
		Title:             m.Title,
		Repos:             logV1MetaRepoFromData(m.Repos),
		Harness:           m.Harness,
		RequestedModel:    m.RequestedModel,
		RequestedEffort:   m.RequestedEffort,
		StartedAt:         m.StartedAt,
		ForgeIssue:        m.ForgeIssue,
		OwnerID:           m.OwnerID,
		ForkedFromTaskID:  m.ForkedFromTaskID,
		ParentTaskID:      m.ParentTaskID,
		CaicMCP:           m.CaicMCP,
		Tailscale:         m.Tailscale,
		USB:               m.USB,
		Display:           m.Display,
		Sudo:              m.Sudo,
		GitHubToken:       m.GitHubToken,
		RuntimeName:       m.RuntimeName,
		BaseImage:         m.BaseImage,
		ContainerPlatform: m.ContainerPlatform,
		MaxCPUs:           m.MaxCPUs,
		CacheMounts:       logV1MetaCacheMountFromData(m.CacheMounts),
		Mounts:            logV1MetaMountFromData(m.Mounts),
	}
}

func logV1DiffStatMessageFromData(m *v1.DiffStatMessage) DiffStatMessage {
	return DiffStatMessage{
		MessageType: m.MessageType,
		DiffStat:    logV1DiffFileStatFromData(m.DiffStat),
		Repos:       logV1RepoStateFromData(m.Repos),
		Ts:          m.Ts,
	}
}

func logV1MetaResultMessageFromData(m *v1.MetaResultMessage) MetaResultMessage {
	return MetaResultMessage{
		MessageType:              m.MessageType,
		State:                    m.State,
		Title:                    m.Title,
		CostUSD:                  m.CostUSD,
		Duration:                 m.Duration,
		NumTurns:                 m.NumTurns,
		InputTokens:              m.InputTokens,
		OutputTokens:             m.OutputTokens,
		CacheCreationInputTokens: m.CacheCreationInputTokens,
		CacheReadInputTokens:     m.CacheReadInputTokens,
		ReasoningOutputTokens:    m.ReasoningOutputTokens,
		DiffStat:                 logV1DiffFileStatFromData(m.DiffStat),
		DiskUsedBytes:            m.DiskUsedBytes,
		Error:                    m.Error,
		AgentResult:              m.AgentResult,
		StartupFailure:           logV1StartupFailureFromData(m.StartupFailure),
	}
}

func logV1TurnCommitSnapshotMessageFromData(m *v1.TurnCommitSnapshotMessage) TurnCommitSnapshotMessage {
	return TurnCommitSnapshotMessage{
		MessageType:       m.MessageType,
		Baseline:          m.Baseline,
		RepositoryCommits: logV1RepositoryCommitFromData(m.RepositoryCommits),
		ChangeStat:        logV1ChangeStatFromData(m.ChangeStat),
	}
}

func logV1PendingUserActionMessageFromData(m *v1.PendingUserActionMessage) PendingUserActionMessage {
	return PendingUserActionMessage{
		MessageType: m.MessageType,
		Action:      logV1PendingUserActionFromData(&m.Action),
	}
}

func logV1AskQuestionFromDataValue(m *v1.AskQuestion) v3.AskQuestion {
	return v3.AskQuestion{
		Question:    m.Question,
		Header:      m.Header,
		Options:     logV1AskOptionFromData(m.Options),
		MultiSelect: m.MultiSelect,
	}
}

func logV1PendingAskActionFromData(m *v1.PendingAskAction) v3.PendingAskAction {
	return v3.PendingAskAction{
		Questions: logV1AskQuestionFromData(m.Questions),
	}
}

func logV1PendingUserActionFromData(m *v1.PendingUserAction) v3.PendingUserAction {
	return v3.PendingUserAction{
		Kind:      v3.PendingUserActionKind(m.Kind),
		RequestID: m.RequestID,
		ToolUseID: m.ToolUseID,
		Ask:       logV1PendingAskActionFromData(&m.Ask),
	}
}

func logV1DiffFileStatFromData(in []v1.DiffFileStat) []v3.DiffFileStat {
	if in == nil {
		return nil
	}
	out := make([]v3.DiffFileStat, len(in))
	for i := range in {
		out[i] = v3.DiffFileStat(in[i])
	}
	return out
}

func logV1RepoStateFromData(in []v1.RepoState) []v3.RepoState {
	if in == nil {
		return nil
	}
	out := make([]v3.RepoState, len(in))
	for i := range in {
		out[i] = v3.RepoState(in[i])
	}
	return out
}

func logV1MetaRepoFromData(in []v1.MetaRepo) []v3.MetaRepo {
	if in == nil {
		return nil
	}
	out := make([]v3.MetaRepo, len(in))
	for i := range in {
		out[i] = v3.MetaRepo(in[i])
	}
	return out
}

func logV1MetaCacheMountFromData(in []v1.MetaCacheMount) []v3.MetaCacheMount {
	if in == nil {
		return nil
	}
	out := make([]v3.MetaCacheMount, len(in))
	for i := range in {
		out[i] = v3.MetaCacheMount(in[i])
	}
	return out
}

func logV1MetaMountFromData(in []v1.MetaMount) []v3.MetaMount {
	if in == nil {
		return nil
	}
	out := make([]v3.MetaMount, len(in))
	for i := range in {
		out[i] = v3.MetaMount(in[i])
	}
	return out
}

func logV1StartupFailureFromData(in *v1.StartupFailure) *v3.StartupFailure {
	if in == nil {
		return nil
	}
	out := new(v3.StartupFailure)
	*out = v3.StartupFailure(*in)
	return out
}

func logV1RepositoryCommitFromData(in []v1.RepositoryCommit) []v3.RepositoryCommit {
	if in == nil {
		return nil
	}
	out := make([]v3.RepositoryCommit, len(in))
	for i := range in {
		out[i] = v3.RepositoryCommit(in[i])
	}
	return out
}

func logV1ChangeStatFromData(in *v1.ChangeStat) *v3.ChangeStat {
	if in == nil {
		return nil
	}
	out := new(v3.ChangeStat)
	*out = v3.ChangeStat(*in)
	return out
}

func logV1AskOptionFromData(in []v1.AskOption) []v3.AskOption {
	if in == nil {
		return nil
	}
	out := make([]v3.AskOption, len(in))
	for i := range in {
		out[i] = v3.AskOption(in[i])
	}
	return out
}

func logV1AskQuestionFromData(in []v1.AskQuestion) []v3.AskQuestion {
	if in == nil {
		return nil
	}
	out := make([]v3.AskQuestion, len(in))
	for i := range in {
		out[i] = logV1AskQuestionFromDataValue(&in[i])
	}
	return out
}

func logV2UserInputMessageToData(m *UserInputMessage, token logRecordType) v2.UserInputMessage {
	return v2.UserInputMessage{
		MessageType: string(token),
		Text:        m.Text,
		Images:      logV2ImageDataToData(m.Images),
	}
}

func logV2UserInputMessageFromData(m *v2.UserInputMessage) UserInputMessage {
	return UserInputMessage{
		Text:   m.Text,
		Images: logV2ImageDataFromData(m.Images),
	}
}

func logV2MetaMessageToData(m *MetaMessage, token logRecordType) v2.MetaMessage {
	return v2.MetaMessage{
		MessageType:       string(token),
		Version:           m.Version,
		Prompt:            m.Prompt,
		Title:             m.Title,
		Repos:             logV2MetaRepoToData(m.Repos),
		Harness:           m.Harness,
		RequestedModel:    m.RequestedModel,
		RequestedEffort:   m.RequestedEffort,
		StartedAt:         m.StartedAt,
		ForgeIssue:        m.ForgeIssue,
		OwnerID:           m.OwnerID,
		ForkedFromTaskID:  m.ForkedFromTaskID,
		ParentTaskID:      m.ParentTaskID,
		CaicMCP:           m.CaicMCP,
		Tailscale:         m.Tailscale,
		USB:               m.USB,
		Display:           m.Display,
		Sudo:              m.Sudo,
		GitHubToken:       m.GitHubToken,
		RuntimeName:       m.RuntimeName,
		BaseImage:         m.BaseImage,
		ContainerPlatform: m.ContainerPlatform,
		MaxCPUs:           m.MaxCPUs,
		CacheMounts:       logV2MetaCacheMountToData(m.CacheMounts),
		Mounts:            logV2MetaMountToData(m.Mounts),
	}
}

func logV2MetaMessageFromData(m *v2.MetaMessage) MetaMessage {
	return MetaMessage{
		MessageType:       m.MessageType,
		Version:           m.Version,
		Prompt:            m.Prompt,
		Title:             m.Title,
		Repos:             logV2MetaRepoFromData(m.Repos),
		Harness:           m.Harness,
		RequestedModel:    m.RequestedModel,
		RequestedEffort:   m.RequestedEffort,
		StartedAt:         m.StartedAt,
		ForgeIssue:        m.ForgeIssue,
		OwnerID:           m.OwnerID,
		ForkedFromTaskID:  m.ForkedFromTaskID,
		ParentTaskID:      m.ParentTaskID,
		CaicMCP:           m.CaicMCP,
		Tailscale:         m.Tailscale,
		USB:               m.USB,
		Display:           m.Display,
		Sudo:              m.Sudo,
		GitHubToken:       m.GitHubToken,
		RuntimeName:       m.RuntimeName,
		BaseImage:         m.BaseImage,
		ContainerPlatform: m.ContainerPlatform,
		MaxCPUs:           m.MaxCPUs,
		CacheMounts:       logV2MetaCacheMountFromData(m.CacheMounts),
		Mounts:            logV2MetaMountFromData(m.Mounts),
	}
}

func logV2DiffStatMessageToData(m *DiffStatMessage, token logRecordType) v2.DiffStatMessage {
	return v2.DiffStatMessage{
		MessageType: string(token),
		DiffStat:    logV2DiffFileStatToData(m.DiffStat),
		Repos:       logV2RepoStateToData(m.Repos),
		Ts:          m.Ts,
	}
}

func logV2DiffStatMessageFromData(m *v2.DiffStatMessage) DiffStatMessage {
	return DiffStatMessage{
		MessageType: m.MessageType,
		DiffStat:    logV2DiffFileStatFromData(m.DiffStat),
		Repos:       logV2RepoStateFromData(m.Repos),
		Ts:          m.Ts,
	}
}

func logV2MetaResultMessageToData(m *MetaResultMessage, token logRecordType) v2.MetaResultMessage {
	return v2.MetaResultMessage{
		MessageType:              string(token),
		State:                    m.State,
		Title:                    m.Title,
		CostUSD:                  m.CostUSD,
		Duration:                 m.Duration,
		NumTurns:                 m.NumTurns,
		InputTokens:              m.InputTokens,
		OutputTokens:             m.OutputTokens,
		CacheCreationInputTokens: m.CacheCreationInputTokens,
		CacheReadInputTokens:     m.CacheReadInputTokens,
		ReasoningOutputTokens:    m.ReasoningOutputTokens,
		DiffStat:                 logV2DiffFileStatToData(m.DiffStat),
		DiskUsedBytes:            m.DiskUsedBytes,
		Error:                    m.Error,
		AgentResult:              m.AgentResult,
		StartupFailure:           logV2StartupFailureToData(m.StartupFailure),
	}
}

func logV2MetaResultMessageFromData(m *v2.MetaResultMessage) MetaResultMessage {
	return MetaResultMessage{
		MessageType:              m.MessageType,
		State:                    m.State,
		Title:                    m.Title,
		CostUSD:                  m.CostUSD,
		Duration:                 m.Duration,
		NumTurns:                 m.NumTurns,
		InputTokens:              m.InputTokens,
		OutputTokens:             m.OutputTokens,
		CacheCreationInputTokens: m.CacheCreationInputTokens,
		CacheReadInputTokens:     m.CacheReadInputTokens,
		ReasoningOutputTokens:    m.ReasoningOutputTokens,
		DiffStat:                 logV2DiffFileStatFromData(m.DiffStat),
		DiskUsedBytes:            m.DiskUsedBytes,
		Error:                    m.Error,
		AgentResult:              m.AgentResult,
		StartupFailure:           logV2StartupFailureFromData(m.StartupFailure),
	}
}

func logV2TurnCommitSnapshotMessageToData(m *TurnCommitSnapshotMessage, token logRecordType) v2.TurnCommitSnapshotMessage {
	return v2.TurnCommitSnapshotMessage{
		MessageType:       string(token),
		Baseline:          m.Baseline,
		RepositoryCommits: logV2RepositoryCommitToData(m.RepositoryCommits),
		ChangeStat:        logV2ChangeStatToData(m.ChangeStat),
	}
}

func logV2TurnCommitSnapshotMessageFromData(m *v2.TurnCommitSnapshotMessage) TurnCommitSnapshotMessage {
	return TurnCommitSnapshotMessage{
		MessageType:       m.MessageType,
		Baseline:          m.Baseline,
		RepositoryCommits: logV2RepositoryCommitFromData(m.RepositoryCommits),
		ChangeStat:        logV2ChangeStatFromData(m.ChangeStat),
	}
}

func logV2PendingUserActionMessageToData(m *PendingUserActionMessage, token logRecordType) v2.PendingUserActionMessage {
	return v2.PendingUserActionMessage{
		MessageType: string(token),
		Action:      logV2PendingUserActionToData(&m.Action),
	}
}

func logV2PendingUserActionMessageFromData(m *v2.PendingUserActionMessage) PendingUserActionMessage {
	return PendingUserActionMessage{
		MessageType: m.MessageType,
		Action:      logV2PendingUserActionFromData(&m.Action),
	}
}

func logV2MCPRequestMessageFromData(m *v2.MCPRequestMessage) MCPRequestMessage {
	return MCPRequestMessage{
		ID:        m.ID,
		Method:    m.Method,
		Name:      m.Name,
		Arguments: m.Arguments,
	}
}

func logV2AskQuestionToDataValue(m *v3.AskQuestion) v2.AskQuestion {
	return v2.AskQuestion{
		Question:    m.Question,
		Header:      m.Header,
		Options:     logV2AskOptionToData(m.Options),
		MultiSelect: m.MultiSelect,
	}
}

func logV2AskQuestionFromDataValue(m *v2.AskQuestion) v3.AskQuestion {
	return v3.AskQuestion{
		Question:    m.Question,
		Header:      m.Header,
		Options:     logV2AskOptionFromData(m.Options),
		MultiSelect: m.MultiSelect,
	}
}

func logV2PendingAskActionToData(m *v3.PendingAskAction) v2.PendingAskAction {
	return v2.PendingAskAction{
		Questions: logV2AskQuestionToData(m.Questions),
	}
}

func logV2PendingAskActionFromData(m *v2.PendingAskAction) v3.PendingAskAction {
	return v3.PendingAskAction{
		Questions: logV2AskQuestionFromData(m.Questions),
	}
}

func logV2PendingUserActionToData(m *v3.PendingUserAction) v2.PendingUserAction {
	return v2.PendingUserAction{
		Kind:      string(m.Kind),
		RequestID: m.RequestID,
		ToolUseID: m.ToolUseID,
		Ask:       logV2PendingAskActionToData(&m.Ask),
	}
}

func logV2PendingUserActionFromData(m *v2.PendingUserAction) v3.PendingUserAction {
	return v3.PendingUserAction{
		Kind:      v3.PendingUserActionKind(m.Kind),
		RequestID: m.RequestID,
		ToolUseID: m.ToolUseID,
		Ask:       logV2PendingAskActionFromData(&m.Ask),
	}
}

func logV2DiffFileStatToData(in []v3.DiffFileStat) []v2.DiffFileStat {
	if in == nil {
		return nil
	}
	out := make([]v2.DiffFileStat, len(in))
	for i := range in {
		out[i] = v2.DiffFileStat(in[i])
	}
	return out
}

func logV2DiffFileStatFromData(in []v2.DiffFileStat) []v3.DiffFileStat {
	if in == nil {
		return nil
	}
	out := make([]v3.DiffFileStat, len(in))
	for i := range in {
		out[i] = v3.DiffFileStat(in[i])
	}
	return out
}

func logV2RepoStateToData(in []v3.RepoState) []v2.RepoState {
	if in == nil {
		return nil
	}
	out := make([]v2.RepoState, len(in))
	for i := range in {
		out[i] = v2.RepoState(in[i])
	}
	return out
}

func logV2RepoStateFromData(in []v2.RepoState) []v3.RepoState {
	if in == nil {
		return nil
	}
	out := make([]v3.RepoState, len(in))
	for i := range in {
		out[i] = v3.RepoState(in[i])
	}
	return out
}

func logV2MetaRepoToData(in []v3.MetaRepo) []v2.MetaRepo {
	if in == nil {
		return nil
	}
	out := make([]v2.MetaRepo, len(in))
	for i := range in {
		out[i] = v2.MetaRepo(in[i])
	}
	return out
}

func logV2MetaRepoFromData(in []v2.MetaRepo) []v3.MetaRepo {
	if in == nil {
		return nil
	}
	out := make([]v3.MetaRepo, len(in))
	for i := range in {
		out[i] = v3.MetaRepo(in[i])
	}
	return out
}

func logV2MetaCacheMountToData(in []v3.MetaCacheMount) []v2.MetaCacheMount {
	if in == nil {
		return nil
	}
	out := make([]v2.MetaCacheMount, len(in))
	for i := range in {
		out[i] = v2.MetaCacheMount(in[i])
	}
	return out
}

func logV2MetaCacheMountFromData(in []v2.MetaCacheMount) []v3.MetaCacheMount {
	if in == nil {
		return nil
	}
	out := make([]v3.MetaCacheMount, len(in))
	for i := range in {
		out[i] = v3.MetaCacheMount(in[i])
	}
	return out
}

func logV2MetaMountToData(in []v3.MetaMount) []v2.MetaMount {
	if in == nil {
		return nil
	}
	out := make([]v2.MetaMount, len(in))
	for i := range in {
		out[i] = v2.MetaMount(in[i])
	}
	return out
}

func logV2MetaMountFromData(in []v2.MetaMount) []v3.MetaMount {
	if in == nil {
		return nil
	}
	out := make([]v3.MetaMount, len(in))
	for i := range in {
		out[i] = v3.MetaMount(in[i])
	}
	return out
}

func logV2StartupFailureToData(in *v3.StartupFailure) *v2.StartupFailure {
	if in == nil {
		return nil
	}
	out := new(v2.StartupFailure)
	*out = v2.StartupFailure(*in)
	return out
}

func logV2StartupFailureFromData(in *v2.StartupFailure) *v3.StartupFailure {
	if in == nil {
		return nil
	}
	out := new(v3.StartupFailure)
	*out = v3.StartupFailure(*in)
	return out
}

func logV2RepositoryCommitToData(in []v3.RepositoryCommit) []v2.RepositoryCommit {
	if in == nil {
		return nil
	}
	out := make([]v2.RepositoryCommit, len(in))
	for i := range in {
		out[i] = v2.RepositoryCommit(in[i])
	}
	return out
}

func logV2RepositoryCommitFromData(in []v2.RepositoryCommit) []v3.RepositoryCommit {
	if in == nil {
		return nil
	}
	out := make([]v3.RepositoryCommit, len(in))
	for i := range in {
		out[i] = v3.RepositoryCommit(in[i])
	}
	return out
}

func logV2ChangeStatToData(in *v3.ChangeStat) *v2.ChangeStat {
	if in == nil {
		return nil
	}
	out := new(v2.ChangeStat)
	*out = v2.ChangeStat(*in)
	return out
}

func logV2ChangeStatFromData(in *v2.ChangeStat) *v3.ChangeStat {
	if in == nil {
		return nil
	}
	out := new(v3.ChangeStat)
	*out = v3.ChangeStat(*in)
	return out
}

func logV2AskOptionToData(in []v3.AskOption) []v2.AskOption {
	if in == nil {
		return nil
	}
	out := make([]v2.AskOption, len(in))
	for i := range in {
		out[i] = v2.AskOption(in[i])
	}
	return out
}

func logV2AskOptionFromData(in []v2.AskOption) []v3.AskOption {
	if in == nil {
		return nil
	}
	out := make([]v3.AskOption, len(in))
	for i := range in {
		out[i] = v3.AskOption(in[i])
	}
	return out
}

func logV2AskQuestionToData(in []v3.AskQuestion) []v2.AskQuestion {
	if in == nil {
		return nil
	}
	out := make([]v2.AskQuestion, len(in))
	for i := range in {
		out[i] = logV2AskQuestionToDataValue(&in[i])
	}
	return out
}

func logV2AskQuestionFromData(in []v2.AskQuestion) []v3.AskQuestion {
	if in == nil {
		return nil
	}
	out := make([]v3.AskQuestion, len(in))
	for i := range in {
		out[i] = logV2AskQuestionFromDataValue(&in[i])
	}
	return out
}

func logV2ImageDataToData(in []v3.ImageData) []v2.ImageData {
	if in == nil {
		return nil
	}
	out := make([]v2.ImageData, len(in))
	for i := range in {
		out[i] = v2.ImageData(in[i])
	}
	return out
}

func logV2ImageDataFromData(in []v2.ImageData) []v3.ImageData {
	if in == nil {
		return nil
	}
	out := make([]v3.ImageData, len(in))
	for i := range in {
		out[i] = v3.ImageData(in[i])
	}
	return out
}

func marshalV2Control(m Message) ([]byte, error) {
	token, err := v2ControlToken(m)
	if err != nil {
		return nil, err
	}
	var record any
	switch m := m.(type) {
	case *TextMessage:
		r := v2.TextMessage(*m)
		r.MessageType = string(token)
		record = r
	case *UserInputMessage:
		r := logV2UserInputMessageToData(m, token)
		r.MessageType = string(token)
		record = r
	case *SystemMessage:
		r := v2.SystemMessage(*m)
		r.MessageType = string(token)
		record = r
	case *LogMessage:
		r := v2.LogMessage(*m)
		r.MessageType = string(token)
		record = r
	case *StrippedEnvMessage:
		r := v2.StrippedEnvMessage(*m)
		r.MessageType = string(token)
		record = r
	case *DiffStatMessage:
		r := logV2DiffStatMessageToData(m, token)
		r.MessageType = string(token)
		record = r
	case *ExitMessage:
		r := v2.ExitMessage(*m)
		r.MessageType = string(token)
		record = r
	case *MetaMessage:
		r := logV2MetaMessageToData(m, token)
		r.MessageType = string(token)
		record = r
	case *MetaSessionMessage:
		r := v2.MetaSessionMessage(*m)
		r.MessageType = string(token)
		record = r
	case *RelayGenerationMessage:
		r := v2.RelayGenerationMessage(*m)
		r.MessageType = string(token)
		record = r
	case *ModelInfoMessage:
		r := v2.ModelInfoMessage(*m)
		r.MessageType = string(token)
		record = r
	case *MetaResultMessage:
		r := logV2MetaResultMessageToData(m, token)
		r.MessageType = string(token)
		record = r
	case *MetaPRMessage:
		r := v2.MetaPRMessage(*m)
		r.MessageType = string(token)
		record = r
	case *TurnCommitSnapshotMessage:
		r := logV2TurnCommitSnapshotMessageToData(m, token)
		r.MessageType = string(token)
		record = r
	case *PendingUserActionMessage:
		r := logV2PendingUserActionMessageToData(m, token)
		r.MessageType = string(token)
		record = r
	default:
		return nil, fmt.Errorf("message type %q is not a task-log control", m.Type())
	}
	// Released Go controls use lexicographically sorted keys.
	data, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

func marshalV3Control(m Message) ([]byte, error) {
	token, err := v2ControlToken(m)
	if err != nil {
		return nil, err
	}
	var record any
	switch m := m.(type) {
	case *TextMessage:
		r := v3.TextMessage(*m)
		r.MessageType = string(token)
		record = r
	case *UserInputMessage:
		r := v3.UserInputMessage(*m)
		r.MessageType = string(token)
		record = r
	case *SystemMessage:
		r := v3.SystemMessage(*m)
		r.MessageType = string(token)
		record = r
	case *LogMessage:
		r := v3.LogMessage(*m)
		r.MessageType = string(token)
		record = r
	case *StrippedEnvMessage:
		r := v3.StrippedEnvMessage(*m)
		r.MessageType = string(token)
		record = r
	case *DiffStatMessage:
		r := v3.DiffStatMessage(*m)
		r.MessageType = string(token)
		record = r
	case *ExitMessage:
		r := v3.ExitMessage(*m)
		r.MessageType = string(token)
		record = r
	case *MetaMessage:
		r := v3.MetaMessage(*m)
		r.MessageType = string(token)
		record = r
	case *MetaSessionMessage:
		r := v3.MetaSessionMessage(*m)
		r.MessageType = string(token)
		record = r
	case *RelayGenerationMessage:
		r := v3.RelayGenerationMessage(*m)
		r.MessageType = string(token)
		record = r
	case *ModelInfoMessage:
		r := v3.ModelInfoMessage(*m)
		r.MessageType = string(token)
		record = r
	case *MetaResultMessage:
		r := v3.MetaResultMessage(*m)
		r.MessageType = string(token)
		record = r
	case *MetaPRMessage:
		r := v3.MetaPRMessage(*m)
		r.MessageType = string(token)
		record = r
	case *TurnCommitSnapshotMessage:
		r := v3.TurnCommitSnapshotMessage(*m)
		r.MessageType = string(token)
		record = r
	case *PendingUserActionMessage:
		r := v3.PendingUserActionMessage(*m)
		r.MessageType = string(token)
		record = r
	default:
		return nil, fmt.Errorf("message type %q is not a task-log control", m.Type())
	}
	// Released Go controls use lexicographically sorted keys.
	data, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

func decodeLogData[T any](line []byte, strict bool) (T, error) {
	var record T
	if !strict {
		err := json.Unmarshal(line, &record)
		return record, err
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&record)
	return record, err
}
func decodeLogControl(version LogVersion, line []byte, out Message) error {
	switch version {
	case LogVersionV1:
		switch out := out.(type) {
		case *LogMessage:
			record, err := decodeLogData[v1.LogMessage](line, false)
			if err != nil {
				return err
			}
			*out = LogMessage(record)
		case *StrippedEnvMessage:
			record, err := decodeLogData[v1.StrippedEnvMessage](line, false)
			if err != nil {
				return err
			}
			*out = StrippedEnvMessage(record)
		case *DiffStatMessage:
			record, err := decodeLogData[v1.DiffStatMessage](line, false)
			if err != nil {
				return err
			}
			*out = logV1DiffStatMessageFromData(&record)
		case *ExitMessage:
			record, err := decodeLogData[v1.ExitMessage](line, false)
			if err != nil {
				return err
			}
			*out = ExitMessage(record)
		case *ModelInfoMessage:
			record, err := decodeLogData[v1.ModelInfoMessage](line, false)
			if err != nil {
				return err
			}
			*out = ModelInfoMessage(record)
		case *MetaResultMessage:
			record, err := decodeLogData[v1.MetaResultMessage](line, false)
			if err != nil {
				return err
			}
			*out = logV1MetaResultMessageFromData(&record)
		case *MetaPRMessage:
			record, err := decodeLogData[v1.MetaPRMessage](line, false)
			if err != nil {
				return err
			}
			*out = MetaPRMessage(record)
		case *TurnCommitSnapshotMessage:
			record, err := decodeLogData[v1.TurnCommitSnapshotMessage](line, false)
			if err != nil {
				return err
			}
			*out = logV1TurnCommitSnapshotMessageFromData(&record)
		case *PendingUserActionMessage:
			record, err := decodeLogData[v1.PendingUserActionMessage](line, false)
			if err != nil {
				return err
			}
			*out = logV1PendingUserActionMessageFromData(&record)
		default:
			return fmt.Errorf("unsupported task-log control %T", out)
		}
	case LogVersionV2:
		switch out := out.(type) {
		case *TextMessage:
			record, err := decodeLogData[v2.TextMessage](line, false)
			if err != nil {
				return err
			}
			*out = TextMessage(record)
		case *UserInputMessage:
			record, err := decodeLogData[v2.UserInputMessage](line, false)
			if err != nil {
				return err
			}
			*out = logV2UserInputMessageFromData(&record)
		case *SystemMessage:
			record, err := decodeLogData[v2.SystemMessage](line, false)
			if err != nil {
				return err
			}
			*out = SystemMessage(record)
		case *LogMessage:
			record, err := decodeLogData[v2.LogMessage](line, false)
			if err != nil {
				return err
			}
			*out = LogMessage(record)
		case *StrippedEnvMessage:
			record, err := decodeLogData[v2.StrippedEnvMessage](line, false)
			if err != nil {
				return err
			}
			*out = StrippedEnvMessage(record)
		case *DiffStatMessage:
			record, err := decodeLogData[v2.DiffStatMessage](line, false)
			if err != nil {
				return err
			}
			*out = logV2DiffStatMessageFromData(&record)
		case *ExitMessage:
			record, err := decodeLogData[v2.ExitMessage](line, false)
			if err != nil {
				return err
			}
			*out = ExitMessage(record)
		case *MetaMessage:
			record, err := decodeLogData[v2.MetaMessage](line, true)
			if err != nil {
				return err
			}
			*out = logV2MetaMessageFromData(&record)
		case *MetaSessionMessage:
			record, err := decodeLogData[v2.MetaSessionMessage](line, false)
			if err != nil {
				return err
			}
			*out = MetaSessionMessage(record)
		case *RelayGenerationMessage:
			record, err := decodeLogData[v2.RelayGenerationMessage](line, false)
			if err != nil {
				return err
			}
			*out = RelayGenerationMessage(record)
		case *ModelInfoMessage:
			record, err := decodeLogData[v2.ModelInfoMessage](line, false)
			if err != nil {
				return err
			}
			*out = ModelInfoMessage(record)
		case *MetaResultMessage:
			record, err := decodeLogData[v2.MetaResultMessage](line, false)
			if err != nil {
				return err
			}
			*out = logV2MetaResultMessageFromData(&record)
		case *MetaPRMessage:
			record, err := decodeLogData[v2.MetaPRMessage](line, false)
			if err != nil {
				return err
			}
			*out = MetaPRMessage(record)
		case *TurnCommitSnapshotMessage:
			record, err := decodeLogData[v2.TurnCommitSnapshotMessage](line, false)
			if err != nil {
				return err
			}
			*out = logV2TurnCommitSnapshotMessageFromData(&record)
		case *PendingUserActionMessage:
			record, err := decodeLogData[v2.PendingUserActionMessage](line, false)
			if err != nil {
				return err
			}
			*out = logV2PendingUserActionMessageFromData(&record)
		case *MCPRequestMessage:
			record, err := decodeLogData[v2.MCPRequestMessage](line, false)
			if err != nil {
				return err
			}
			*out = logV2MCPRequestMessageFromData(&record)
		default:
			return fmt.Errorf("unsupported task-log control %T", out)
		}
	case LogVersionV3:
		switch out := out.(type) {
		case *TextMessage:
			record, err := decodeLogData[v3.TextMessage](line, false)
			if err != nil {
				return err
			}
			*out = TextMessage(record)
		case *UserInputMessage:
			record, err := decodeLogData[v3.UserInputMessage](line, false)
			if err != nil {
				return err
			}
			*out = UserInputMessage(record)
		case *SystemMessage:
			record, err := decodeLogData[v3.SystemMessage](line, false)
			if err != nil {
				return err
			}
			*out = SystemMessage(record)
		case *LogMessage:
			record, err := decodeLogData[v3.LogMessage](line, false)
			if err != nil {
				return err
			}
			*out = LogMessage(record)
		case *StrippedEnvMessage:
			record, err := decodeLogData[v3.StrippedEnvMessage](line, false)
			if err != nil {
				return err
			}
			*out = StrippedEnvMessage(record)
		case *DiffStatMessage:
			record, err := decodeLogData[v3.DiffStatMessage](line, false)
			if err != nil {
				return err
			}
			*out = DiffStatMessage(record)
		case *ExitMessage:
			record, err := decodeLogData[v3.ExitMessage](line, false)
			if err != nil {
				return err
			}
			*out = ExitMessage(record)
		case *MetaMessage:
			record, err := decodeLogData[v3.MetaMessage](line, true)
			if err != nil {
				return err
			}
			*out = MetaMessage(record)
		case *MetaSessionMessage:
			record, err := decodeLogData[v3.MetaSessionMessage](line, false)
			if err != nil {
				return err
			}
			*out = MetaSessionMessage(record)
		case *RelayGenerationMessage:
			record, err := decodeLogData[v3.RelayGenerationMessage](line, false)
			if err != nil {
				return err
			}
			*out = RelayGenerationMessage(record)
		case *ModelInfoMessage:
			record, err := decodeLogData[v3.ModelInfoMessage](line, false)
			if err != nil {
				return err
			}
			*out = ModelInfoMessage(record)
		case *MetaResultMessage:
			record, err := decodeLogData[v3.MetaResultMessage](line, false)
			if err != nil {
				return err
			}
			*out = MetaResultMessage(record)
		case *MetaPRMessage:
			record, err := decodeLogData[v3.MetaPRMessage](line, false)
			if err != nil {
				return err
			}
			*out = MetaPRMessage(record)
		case *TurnCommitSnapshotMessage:
			record, err := decodeLogData[v3.TurnCommitSnapshotMessage](line, false)
			if err != nil {
				return err
			}
			*out = TurnCommitSnapshotMessage(record)
		case *PendingUserActionMessage:
			record, err := decodeLogData[v3.PendingUserActionMessage](line, false)
			if err != nil {
				return err
			}
			*out = PendingUserActionMessage(record)
		case *MCPRequestMessage:
			record, err := decodeLogData[v3.MCPRequestMessage](line, false)
			if err != nil {
				return err
			}
			*out = MCPRequestMessage(record)
		default:
			return fmt.Errorf("unsupported task-log control %T", out)
		}
	default:
		return version.Validate()
	}
	return nil
}
