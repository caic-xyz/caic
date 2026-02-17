// Package fake embeds the fake agent Python script for e2e testing.
package fake

import _ "embed"

// Script is the fake agent that cycles through jokes in Claude Code streaming JSON format.
//
//go:embed fake_agent.py
var Script []byte
