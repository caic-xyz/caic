// MCP Skills extension configuration and embedded caic task-management skill.

package server

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/caic-xyz/caic/backend/internal/mcp"
)

const (
	mcpTasksSkillName = "tasks"
	mcpTasksSkillURI  = "skill://tasks/SKILL.md"
)

var (
	//go:embed skills/tasks/SKILL.md
	mcpTasksSkillMarkdown string
)

// loadMCPTaskSkills loads caic's embedded, static Agent Skills.
func loadMCPTaskSkills() ([]mcp.Skill, error) {
	skill, err := parseMCPTaskSkill(mcpTasksSkillMarkdown)
	if err != nil {
		return nil, err
	}
	return []mcp.Skill{skill}, nil
}

func parseMCPTaskSkill(markdown string) (mcp.Skill, error) {
	frontmatter, ok := strings.CutPrefix(markdown, "---\n")
	if !ok {
		return mcp.Skill{}, errors.New("MCP tasks skill must begin with YAML frontmatter")
	}
	frontmatter, _, ok = strings.Cut(frontmatter, "\n---\n")
	if !ok {
		return mcp.Skill{}, errors.New("MCP tasks skill YAML frontmatter is unterminated")
	}

	fields := map[string]any{}
	if err := yaml.Unmarshal([]byte(frontmatter), &fields); err != nil {
		return mcp.Skill{}, fmt.Errorf("parse MCP tasks skill YAML frontmatter: %w", err)
	}
	name, ok := fields["name"].(string)
	if !ok || name != mcpTasksSkillName {
		return mcp.Skill{}, fmt.Errorf("MCP tasks skill name = %q, want %q", name, mcpTasksSkillName)
	}
	if description, ok := fields["description"].(string); !ok || description == "" {
		return mcp.Skill{}, errors.New("MCP tasks skill description is required")
	}

	digest := sha256.Sum256([]byte(markdown))
	return mcp.Skill{
		URI:         mcpTasksSkillURI,
		Frontmatter: fields,
		Resources: []mcp.SkillResource{{
			URI:    mcpTasksSkillURI,
			Digest: "sha256:" + hex.EncodeToString(digest[:]),
			Size:   int64(len(markdown)),
		}},
	}, nil
}
