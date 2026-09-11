// Legacy v1 task-log record decoding.

package agent

import (
	"encoding/json"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

// DecodeV1MetaMessage decodes a legacy v1 metadata header into canonical fields.
func DecodeV1MetaMessage(line []byte) (MetaMessage, error) {
	var m MetaMessage
	if err := json.Unmarshal(line, &m); err != nil {
		return MetaMessage{}, err
	}
	return m, nil
}

// DecodeV1MetaSessionMessage decodes legacy v1 session metadata into canonical fields.
func DecodeV1MetaSessionMessage(line []byte) (MetaSessionMessage, error) {
	var m MetaSessionMessage
	if err := json.Unmarshal(line, &m); err != nil {
		return MetaSessionMessage{}, err
	}
	return m, nil
}

func marshalV1LogMessage(m Message) ([]byte, error) {
	data, err := MarshalMessage(m)
	if err != nil {
		return nil, err
	}
	switch m := m.(type) {
	case *MetaMessage:
		return marshalV1MetaMessage(m)
	case *MetaSessionMessage:
		return json.Marshal(v1MetaSessionMessage{
			MessageType:  m.MessageType,
			SessionID:    m.SessionID,
			Model:        m.ReportedModel,
			AgentVersion: m.AgentVersion,
		})
	default:
		return data, nil
	}
}

// v1MetaMessage preserves the obsolete v1 header's JSON schema and field order.
type v1MetaMessage struct {
	MessageType       string           `json:"type"`
	Version           int              `json:"version"`
	Prompt            string           `json:"prompt"`
	Title             string           `json:"title,omitempty"`
	Repos             []MetaRepo       `json:"repos"`
	Harness           harness.Name     `json:"harness"`
	Model             string           `json:"model,omitempty"`
	Effort            string           `json:"effort,omitempty"`
	StartedAt         time.Time        `json:"started_at"`
	ForgeIssue        int              `json:"forge_issue,omitempty"`
	ForkedFromTaskID  string           `json:"forked_from_task_id,omitempty"`
	ParentTaskID      string           `json:"parent_task_id,omitempty"`
	Tailscale         bool             `json:"tailscale,omitempty"`
	USB               bool             `json:"usb,omitempty"`
	Display           bool             `json:"display,omitempty"`
	Sudo              bool             `json:"sudo,omitempty"`
	GitHubToken       bool             `json:"gitHubToken,omitempty"`
	RuntimeName       string           `json:"runtimeName,omitempty"`
	BaseImage         string           `json:"baseImage,omitempty"`
	ContainerPlatform string           `json:"containerPlatform,omitempty"`
	MaxCPUs           int              `json:"maxCPUs,omitempty"`
	CacheMounts       []MetaCacheMount `json:"cacheMounts,omitempty"`
	Mounts            []MetaMount      `json:"mounts,omitempty"`
}

// v1MetaSessionMessage preserves the obsolete v1 session record schema.
type v1MetaSessionMessage struct {
	MessageType  string `json:"type"`
	SessionID    string `json:"session_id"`
	Model        string `json:"model,omitempty"`
	AgentVersion string `json:"agent_version,omitempty"`
}

func marshalV1MetaMessage(m *MetaMessage) ([]byte, error) {
	return json.Marshal(v1MetaMessage{
		MessageType:       m.MessageType,
		Version:           m.Version,
		Prompt:            m.Prompt,
		Title:             m.Title,
		Repos:             m.Repos,
		Harness:           m.Harness,
		Model:             m.RequestedModel,
		Effort:            m.RequestedEffort,
		StartedAt:         m.StartedAt,
		ForgeIssue:        m.ForgeIssue,
		ForkedFromTaskID:  m.ForkedFromTaskID,
		ParentTaskID:      m.ParentTaskID,
		Tailscale:         m.Tailscale,
		USB:               m.USB,
		Display:           m.Display,
		Sudo:              m.Sudo,
		GitHubToken:       m.GitHubToken,
		RuntimeName:       m.RuntimeName,
		BaseImage:         m.BaseImage,
		ContainerPlatform: m.ContainerPlatform,
		MaxCPUs:           m.MaxCPUs,
		CacheMounts:       m.CacheMounts,
		Mounts:            m.Mounts,
	})
}

type v1RecordDiscriminator struct {
	Type string `json:"type"`
}

func parseV1Record(p *LogRecordParser, line []byte) (ParsedRecord, error) {
	var envelope v1RecordDiscriminator
	if err := json.Unmarshal(line, &envelope); err != nil {
		msgs, parseErr := p.parseAndApplyNative(line)
		return ParsedRecord{Messages: wrapParsedMessages(msgs, time.Time{})}, parseErr
	}
	kind, ok := v1LogControlKinds[envelope.Type]
	if ok {
		msgs, err := p.parseControl(kind, envelope.Type, line)
		if err != nil {
			return ParsedRecord{Control: true}, err
		}
		msgs, err = p.applyMessageState(msgs)
		return ParsedRecord{Messages: wrapParsedMessages(msgs, time.Time{}), Control: true}, err
	}
	msgs, err := p.parseAndApplyNative(line)
	return ParsedRecord{Messages: wrapParsedMessages(msgs, time.Time{})}, err
}
