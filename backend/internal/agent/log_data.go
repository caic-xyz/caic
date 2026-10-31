// Encodes and decodes v3 task-log controls as semantic messages.

package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

// MarshalLogMessage encodes one semantic backend control as a v3 record.
func MarshalLogMessage(m Message) ([]byte, error) {
	if _, ok := m.(*RawMessage); ok {
		return nil, errors.New("raw messages must be appended as native records")
	}
	token, err := v3ControlToken(m)
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
		r := logV3MetaResultMessageToData(m, token)
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
func decodeLogControl(line []byte, out Message) error {
	switch out := out.(type) {
	case *TextMessage:
		record, err := decodeLogData[v3.TextMessage](line, true)
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
		*out = logV3MetaResultMessageFromData(&record)
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
	return nil
}

func logV3MetaResultMessageToData(m *MetaResultMessage, token logRecordType) v3.MetaResultMessage {
	return v3.MetaResultMessage{
		MessageProvenance:        m.MessageProvenance,
		MessageType:              string(token),
		State:                    m.State,
		Title:                    m.Title,
		CostUSD:                  m.CostUSD,
		Duration:                 m.Duration,
		NumTurns:                 m.NumTurns,
		InputTokens:              m.InputTokens,
		OutputTokens:             m.OutputTokens,
		CacheCreationInputTokens: m.CacheCreationInputTokens,
		CacheWrites:              m.CacheWrites,
		CacheReadInputTokens:     m.CacheReadInputTokens,
		ReasoningOutputTokens:    m.ReasoningOutputTokens,
		DiffStat:                 m.DiffStat,
		DiskUsedBytes:            m.DiskUsedBytes,
		Error:                    m.Error,
		AgentResult:              m.AgentResult,
		StartupFailure:           m.StartupFailure,
	}
}

func logV3MetaResultMessageFromData(m *v3.MetaResultMessage) MetaResultMessage {
	return MetaResultMessage{
		MessageProvenance:        m.MessageProvenance,
		MessageType:              m.MessageType,
		State:                    m.State,
		Title:                    m.Title,
		CostUSD:                  m.CostUSD,
		Duration:                 m.Duration,
		NumTurns:                 m.NumTurns,
		InputTokens:              m.InputTokens,
		OutputTokens:             m.OutputTokens,
		CacheCreationInputTokens: m.CacheCreationInputTokens,
		CacheWrites:              logCacheWritesFromData(m.CacheCreationInputTokens, m.CacheCreation1hInputTokens, m.CacheWrites),
		CacheReadInputTokens:     m.CacheReadInputTokens,
		ReasoningOutputTokens:    m.ReasoningOutputTokens,
		DiffStat:                 m.DiffStat,
		DiskUsedBytes:            m.DiskUsedBytes,
		Error:                    m.Error,
		AgentResult:              m.AgentResult,
		StartupFailure:           m.StartupFailure,
	}
}

// logCacheWritesFromData translates the stored one-hour subset at the decoding
// boundary. Explicit duration buckets take precedence over that subset.
func logCacheWritesFromData(total, oneHour int, buckets []v3.CacheWrite) []v3.CacheWrite {
	if len(buckets) > 0 || oneHour <= 0 {
		return buckets
	}
	oneHour = min(max(0, total), oneHour)
	out := make([]v3.CacheWrite, 0, 2)
	if remaining := total - oneHour; remaining > 0 {
		out = append(out, v3.CacheWrite{Tokens: int64(remaining)})
	}
	if oneHour > 0 {
		out = append(out, v3.CacheWrite{TTLSeconds: 3600, Tokens: int64(oneHour)})
	}
	return out
}
