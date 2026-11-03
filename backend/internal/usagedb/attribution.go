// Model attribution labels for the daily usage rollup.

package usagedb

import "strings"

// effortWords lists the thinking-effort values harnesses embed in a reported
// model id, as in "claude-sonnet-5-5-medium".
var effortWords = []string{"minimal", "low", "medium", "high", "max", "ultra"}

// ModelLabel canonicalizes one attributed model and its thinking effort into
// the label the rollup keys by. Harnesses report the same model as a bare id,
// with a provider prefix, or with the effort appended, so both forms collapse:
// the prefix and the effort suffix are removed and a known effort is appended
// in parentheses, which keeps effort variants distinguishable.
func ModelLabel(model, effort string) string {
	if i := strings.LastIndexByte(model, '/'); i >= 0 {
		model = model[i+1:]
	}
	for _, word := range effortWords {
		if !strings.HasSuffix(model, "-"+word) {
			continue
		}
		model = strings.TrimSuffix(model, "-"+word)
		if effort == "" {
			effort = word
		}
		break
	}
	if model == "" || effort == "" {
		return model
	}
	return model + " (" + effort + ")"
}
