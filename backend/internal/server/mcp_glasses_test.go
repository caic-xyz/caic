// Tests tasks_show_on_glasses: a PNG card on Halo, text on Even G2, and an error without glasses.

package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"slices"
	"testing"

	"github.com/gomodedev/gomode/mcp"
	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/taskrun/taskruntest"
)

// The displays Go Mode declares for Halo and Even G2.
const (
	haloDisplayJSON = `{"width":256,"height":256,"shape":"circle","colorSpace":"rgb","bitsPerPixel":4,"imageTypes":["image/png"]}`
	g2DisplayJSON   = `{"width":576,"height":288,"shape":"rectangle","colorSpace":"luminance","bitsPerPixel":4}`
)

func TestTasksShowOnGlasses(t *testing.T) {
	t.Parallel()
	// newTools serves tasks in the given states, titled after their prompts.
	newTools := func(t *testing.T, states ...task.State) *mcpTools {
		s := newTestRouter(t, nil)
		for i, state := range states {
			id := ksid.NewID()
			tk := mustNewTask(t, id, agent.Prompt{Text: []string{"Fix the flaky login test", "Add dark mode"}[i%2]}, harness.Claude)
			taskruntest.SetState(tk, state)
			insertTestTask(s, id, tk)
		}
		return &mcpTools{taskSvc: testTaskHandlers(s).taskSvc}
	}
	// call runs the tool with display, a JSON settings object, declared as the
	// connected glasses, or with no glasses when display is empty.
	call := func(t *testing.T, c *mcpTools, display string) mcp.ToolResult[mcp.TextOutput] {
		capabilities := `{}`
		if display != "" {
			capabilities = `{"extensions":{"com.fghbuild.gomode/display":` + display + `}}`
		}
		var meta mcp.RequestMeta
		if err := json.Unmarshal([]byte(`{"io.modelcontextprotocol/clientCapabilities":`+capabilities+`}`), &meta); err != nil {
			t.Fatal(err)
		}
		return c.handleTasksShowOnGlasses(t.Context(), &mcp.ToolRequest[struct{}]{Params: mcp.RequestParams{Meta: meta}})
	}
	t.Run("Halo gets a card in the circle with a dot per state", func(t *testing.T) {
		t.Parallel()
		res := call(t, newTools(t, task.StateRunning, task.StateAsking), haloDisplayJSON)
		if res.IsError || res.Structured != (mcp.TextOutput{Result: "The glasses show all 2 tasks."}) {
			t.Fatalf("result = %+v", res)
		}
		if len(res.Content) != 1 {
			t.Fatalf("content = %+v, want one glasses block", res.Content)
		}
		block := res.Content[0]
		if block.Type != mcp.ContentTypeImage || block.MimeType != "image/png" || !slices.Equal(block.Annotations.Audience, []mcp.Role{mcp.RoleUser}) {
			t.Fatalf("block = %s %s %v, want a PNG for the user alone", block.Type, block.MimeType, block.Annotations.Audience)
		}
		data, err := base64.StdEncoding.DecodeString(block.Data)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if img.Bounds() != image.Rect(0, 0, 256, 256) {
			t.Fatalf("bounds = %v, want Halo's 256x256", img.Bounds())
		}
		var white, green, amber int
		for y := range 256 {
			for x := range 256 {
				r, g, b, _ := img.At(x, y).RGBA()
				r, g, b = r>>8, g>>8, b>>8
				if r+g+b == 0 {
					continue
				}
				if dx, dy := 2*x+1-256, 2*y+1-256; dx*dx+dy*dy > 256*256 {
					t.Fatalf("(%d, %d) is lit outside Halo's circle", x, y)
				}
				switch {
				case r > 200 && g > 200 && b > 200:
					white++
				case g > 2*r && g > 2*b:
					green++
				case r > g && g > 2*b && r > 200:
					amber++
				}
			}
		}
		if white == 0 || green == 0 || amber == 0 {
			t.Fatalf("white text %d, green running dot %d, amber asking dot %d pixels; want each", white, green, amber)
		}
	})
	t.Run("Even G2 gets text, a line per task", func(t *testing.T) {
		t.Parallel()
		res := call(t, newTools(t, task.StateRunning, task.StateStopped), g2DisplayJSON)
		want := mcp.GlassesText("1 running: Fix the flaky login test\n2 stopped: Add dark mode")
		if res.IsError || len(res.Content) != 1 || res.Content[0].Text != want.Text || !slices.Equal(res.Content[0].Annotations.Audience, want.Annotations.Audience) {
			t.Fatalf("result = %+v, want %+v", res, want)
		}
	})
	t.Run("a card holds the first eight tasks", func(t *testing.T) {
		t.Parallel()
		states := slices.Repeat([]task.State{task.StateRunning}, glassesTaskLimit+1)
		res := call(t, newTools(t, states...), g2DisplayJSON)
		if res.IsError || res.Structured != (mcp.TextOutput{Result: "The glasses show tasks 1 to 8 of 9."}) {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("a huge declared display gets a bounded card", func(t *testing.T) {
		t.Parallel()
		res := call(t, newTools(t, task.StateRunning), `{"width":2000000000,"height":1000000000,"shape":"rectangle","colorSpace":"rgb","bitsPerPixel":24,"imageTypes":["image/png"]}`)
		if res.IsError || len(res.Content) != 1 || res.Content[0].Type != mcp.ContentTypeImage {
			t.Fatalf("result = %+v, want an image", res)
		}
		data, err := base64.StdEncoding.DecodeString(res.Content[0].Data)
		if err != nil {
			t.Fatal(err)
		}
		config, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if config.Width != 1024 || config.Height != 512 {
			t.Fatalf("card is %dx%d, want 1024x512: bounded, with the display's aspect ratio", config.Width, config.Height)
		}
	})
	t.Run("a display too thin for legible rows gets text", func(t *testing.T) {
		t.Parallel()
		res := call(t, newTools(t, task.StateRunning), `{"width":2000000000,"height":10,"shape":"rectangle","colorSpace":"rgb","bitsPerPixel":24,"imageTypes":["image/png"]}`)
		if res.IsError || len(res.Content) != 1 || res.Content[0].Type != mcp.ContentTypeText {
			t.Fatalf("result = %+v, want text", res)
		}
	})
	t.Run("no tasks", func(t *testing.T) {
		t.Parallel()
		res := call(t, newTools(t), haloDisplayJSON)
		if res.IsError || res.Structured != (mcp.TextOutput{Result: "The glasses show that there are no tasks."}) || len(res.Content) != 1 {
			t.Fatalf("result = %+v", res)
		}
	})
	for name, display := range map[string]string{"no glasses": "", "a malformed display": `{"width":0}`} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res := call(t, newTools(t, task.StateRunning), display)
			if !res.IsError || res.Structured != (mcp.ErrorOutput{Error: "No glasses are connected."}) || len(res.Content) != 0 {
				t.Fatalf("result = %+v, want an error", res)
			}
		})
	}
}
