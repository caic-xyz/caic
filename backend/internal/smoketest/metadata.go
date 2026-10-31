// Projects fixture metadata without retaining fake-agent answer and transcript fields.

package smoketest

import (
	"encoding/json"

	"github.com/buger/jsonparser"
	"github.com/caic-xyz/caic/backend/internal/agent"
)

type fixtureMetadataParser struct{}

func (*fixtureMetadataParser) ParseInput([]byte) ([]agent.Message, error) { return nil, nil }

func (*fixtureMetadataParser) ParseOutput(line []byte) ([]agent.Message, error) {
	typ, err := agent.JSONString(line, "type")
	if err != nil {
		return nil, err
	}
	var message agent.Message
	switch typ {
	case "init":
		message = &agent.InitMessage{}
	case "usage":
		message = &agent.UsageMessage{}
	case "result":
		message = &agent.ResultMessage{}
	case "system":
		message = &agent.SystemMessage{}
	default:
		return nil, nil
	}
	fields := []byte{'{'}
	err = jsonparser.ObjectEach(line, func(key, value []byte, kind jsonparser.ValueType, _ int) error {
		switch string(key) {
		case "type", "subtype", "is_error", "duration_ms", "duration_api_ms", "num_turns",
			"session_id", "total_cost_usd", "usage", "uuid", "model", "effort", "version",
			"source", "per_call", "model_derived", "additive", "context_window",
			"cumulative_cost_usd", "reported_cost_usd", "auto_compact_token_limit":
		default:
			return nil
		}
		if len(fields) > 1 {
			fields = append(fields, ',')
		}
		fields = append(fields, '"')
		fields = append(fields, key...)
		fields = append(fields, '"', ':')
		if kind == jsonparser.String {
			fields = append(fields, '"')
		}
		fields = append(fields, value...)
		if kind == jsonparser.String {
			fields = append(fields, '"')
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	fields = append(fields, '}')
	if err := json.Unmarshal(fields, message); err != nil {
		return nil, err
	}
	return []agent.Message{message}, nil
}
