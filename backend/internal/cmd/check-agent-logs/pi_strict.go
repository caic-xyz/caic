// Pi strict validation checks nested content blocks in offline Pi protocol logs.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"

	pidto "github.com/maruel/genai/providers/pi"
)

// validatePiStrictContentBlocks validates known Pi message and tool-result
// content paths after the outer Pi record has been decoded strictly.
func validatePiStrictContentBlocks(typ pidto.EventType, data []byte) error {
	switch typ {
	case pidto.EventAgentEnd:
		var raw piAgentEndContentProbe
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		for _, message := range raw.Messages {
			if err := validatePiStrictAgentMessage(message); err != nil {
				return err
			}
		}
	case pidto.EventEntryAppended:
		var raw piEntryAppendedContentProbe
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		return validatePiStrictSessionEntry(raw.Entry)
	case pidto.EventMessageStart, pidto.EventMessageEnd, pidto.EventTurnEnd:
		var raw piMessageEventContentProbe
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		return validatePiStrictAgentMessage(raw.Message)
	case pidto.EventMessageUpdate:
		var raw piMessageEventContentProbe
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		if err := validatePiStrictAgentMessage(raw.Message); err != nil {
			return err
		}
		return validatePiStrictAssistantMessageEvent(raw.AssistantMessageEvent)
	case pidto.EventToolExecUpdate, pidto.EventToolExecEnd:
		var raw piToolExecutionContentProbe
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		if err := validatePiStrictToolExecutionResult(raw.PartialResult); err != nil {
			return err
		}
		return validatePiStrictToolExecutionResult(raw.Result)
	case pidto.EventResponse:
		return validatePiStrictResponse(data)
	default:
		return nil
	}
	return nil
}

// Content probes retain nested JSON until strict decoding can reject unknown
// content-block fields. They are not Pi wire DTOs; use pidto's DTOs for those.
type piResponseContentProbe struct {
	Command pidto.EventType `json:"command"`
	Data    json.RawMessage `json:"data"`
}

type piGetMessagesContentProbe struct {
	Messages []json.RawMessage `json:"messages"`
}

type piEntriesContentProbe struct {
	Entries []json.RawMessage `json:"entries"`
}

type piTreeContentProbe struct {
	Tree []piSessionTreeContentProbe `json:"tree"`
}

type piSessionTreeContentProbe struct {
	Entry    json.RawMessage             `json:"entry"`
	Children []piSessionTreeContentProbe `json:"children"`
}

type piAgentMessageContentProbe struct {
	Content json.RawMessage `json:"content"`
}

type piAgentEndContentProbe struct {
	Messages []json.RawMessage `json:"messages"`
}

type piMessageEventContentProbe struct {
	Message               json.RawMessage `json:"message"`
	AssistantMessageEvent json.RawMessage `json:"assistantMessageEvent"`
}

type piAssistantMessageEventContentProbe struct {
	Error   json.RawMessage `json:"error"`
	Message json.RawMessage `json:"message"`
	Partial json.RawMessage `json:"partial"`
}

type piToolExecutionContentProbe struct {
	PartialResult json.RawMessage `json:"partialResult"`
	Result        json.RawMessage `json:"result"`
}

type piToolExecutionResultContentProbe struct {
	Content json.RawMessage `json:"content"`
}

type piEntryAppendedContentProbe struct {
	Entry json.RawMessage `json:"entry"`
}

type piSessionEntryContentProbe struct {
	Content json.RawMessage `json:"content"`
	Message json.RawMessage `json:"message"`
}

func validatePiStrictResponse(data []byte) error {
	var raw piResponseContentProbe
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	switch raw.Command {
	case pidto.CmdGetMessages:
		var messages piGetMessagesContentProbe
		if err := json.Unmarshal(raw.Data, &messages); err != nil {
			return err
		}
		for _, message := range messages.Messages {
			if err := validatePiStrictAgentMessage(message); err != nil {
				return err
			}
		}
	case pidto.CmdGetEntries:
		var entries piEntriesContentProbe
		if err := json.Unmarshal(raw.Data, &entries); err != nil {
			return err
		}
		for _, entry := range entries.Entries {
			if err := validatePiStrictSessionEntry(entry); err != nil {
				return err
			}
		}
	case pidto.CmdGetTree:
		var tree piTreeContentProbe
		if err := json.Unmarshal(raw.Data, &tree); err != nil {
			return err
		}
		for _, node := range tree.Tree {
			if err := validatePiStrictSessionTreeNode(node); err != nil {
				return err
			}
		}
	default:
		return nil
	}
	return nil
}

func validatePiStrictSessionTreeNode(node piSessionTreeContentProbe) error {
	if err := validatePiStrictSessionEntry(node.Entry); err != nil {
		return err
	}
	for _, child := range node.Children {
		if err := validatePiStrictSessionTreeNode(child); err != nil {
			return err
		}
	}
	return nil
}

func validatePiStrictAgentMessage(data json.RawMessage) error {
	if isEmptyJSON(data) {
		return nil
	}
	var raw piAgentMessageContentProbe
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	return validatePiStrictContentBlockArray(raw.Content)
}

func validatePiStrictAssistantMessageEvent(data json.RawMessage) error {
	if isEmptyJSON(data) {
		return nil
	}
	var raw piAssistantMessageEventContentProbe
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, message := range []json.RawMessage{raw.Error, raw.Message, raw.Partial} {
		if err := validatePiStrictAgentMessage(message); err != nil {
			return err
		}
	}
	return nil
}

func validatePiStrictToolExecutionResult(data json.RawMessage) error {
	if isEmptyJSON(data) {
		return nil
	}
	var raw piToolExecutionResultContentProbe
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	return validatePiStrictContentBlockArray(raw.Content)
}

func validatePiStrictSessionEntry(data json.RawMessage) error {
	if isEmptyJSON(data) {
		return nil
	}
	var raw piSessionEntryContentProbe
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if err := validatePiStrictAgentMessage(raw.Message); err != nil {
		return err
	}
	return validatePiStrictContentBlockArray(raw.Content)
}

func validatePiStrictContentBlockArray(data json.RawMessage) error {
	if !bytes.HasPrefix(bytes.TrimSpace(data), []byte("[")) {
		return nil
	}
	var blocks []pidto.ContentBlock
	if err := strictDecode(data, &blocks); err != nil {
		return fmt.Errorf("content blocks: %w", err)
	}
	return nil
}
