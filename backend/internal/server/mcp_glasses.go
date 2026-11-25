// Draws the tasks_show_on_glasses card: task numbers, states, and titles fitted to the declared glasses display.

package server

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"strings"
	"sync"

	"github.com/gomodedev/gomode"
	"github.com/gomodedev/gomode/mcp"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/mcpschema"
)

// glassesTaskLimit bounds the tasks on one card: glasses are read at a glance.
const glassesTaskLimit = 8

// glassesMaxSide bounds each side of a card in pixels. The client declares the
// display size, and gomode.Display.Validate bounds it only below, so a card
// drawn at the declared size could take any amount of memory. Every glasses
// display is smaller, and the client centres a smaller card.
const glassesMaxSide = 1024

// glassesMinRow is the smallest row, in pixels, whose text stays legible; a
// card with smaller rows is sent as text.
const glassesMinRow = 10

// glassesFont parses the Go font that the card draws with, once.
var glassesFont = sync.OnceValues(func() (*opentype.Font, error) { return opentype.Parse(goregular.TTF) })

// glassesTasksCard returns the glasses block that shows tasks on d: an image
// where d draws PNG images large enough for legible rows, and lines of text
// otherwise, which the client lays out and pages.
func glassesTasksCard(d *gomode.Display, tasks []mcpschema.TaskSummary) (mcp.ContentBlock, error) {
	width, height := glassesCardSize(d)
	if !d.DrawsImages("image/png") || glassesArea(d.Shape, width, height).Dy()/glassesTaskLimit < glassesMinRow {
		return mcp.GlassesText(glassesTasksText(tasks)), nil
	}
	img, err := drawGlassesTasks(d.Shape, width, height, tasks)
	if err != nil {
		return mcp.ContentBlock{}, err
	}
	return mcp.GlassesImage(d, img)
}

// glassesTasksText is one line per task: its number, its state, and its title.
func glassesTasksText(tasks []mcpschema.TaskSummary) string {
	if len(tasks) == 0 {
		return "No tasks"
	}
	lines := make([]string, len(tasks))
	for i := range tasks {
		lines[i] = fmt.Sprintf("%d %s: %s", tasks[i].TaskNumber, tasks[i].State, tasks[i].Title)
	}
	return strings.Join(lines, "\n")
}

// glassesTasksSummary tells the model which of the total tasks the glasses
// show, the first shown in tasks_list order.
func glassesTasksSummary(shown, total int) string {
	switch {
	case total == 0:
		return "The glasses show that there are no tasks."
	case shown == total:
		return fmt.Sprintf("The glasses show all %d tasks.", total)
	default:
		return fmt.Sprintf("The glasses show tasks 1 to %d of %d.", shown, total)
	}
}

// glassesCardSize is the size of d, scaled down to at most glassesMaxSide
// pixels a side with its aspect ratio kept.
func glassesCardSize(d *gomode.Display) (width, height int) {
	longest := max(d.Width, d.Height)
	if longest <= glassesMaxSide {
		return d.Width, d.Height
	}
	scale := float64(glassesMaxSide) / float64(longest)
	return max(1, int(float64(d.Width)*scale)), max(1, int(float64(d.Height)*scale))
}

// drawGlassesTasks draws a width by height card of a row per task on black,
// which additive glasses leave dark: a dot in the colour of its state, then its
// number and title in white, cut to the row. The rows stay inside shape, so on
// a circle they keep to its inscribed square.
func drawGlassesTasks(shape gomode.DisplayShape, width, height int, tasks []mcpschema.TaskSummary) (*image.RGBA, error) {
	f, err := glassesFont()
	if err != nil {
		return nil, err
	}
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), image.Black, image.Point{}, draw.Src)
	area := glassesArea(shape, width, height)
	// Every card splits the area into glassesTaskLimit rows, so the type size
	// does not change with the number of tasks.
	rowHeight := area.Dy() / glassesTaskLimit
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: float64(rowHeight) * 0.7, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, err
	}
	metrics := face.Metrics()
	baseline := (rowHeight + (metrics.Ascent - metrics.Descent).Round()) / 2
	if len(tasks) == 0 {
		text := "No tasks"
		width := font.MeasureString(face, text).Round()
		y := area.Min.Y + (area.Dy()-rowHeight)/2
		drawGlassesText(img, face, text, area.Min.X+(area.Dx()-width)/2, y+baseline)
		return img, nil
	}
	dot := rowHeight / 2
	textX := area.Min.X + dot + dot/2
	top := area.Min.Y + (area.Dy()-len(tasks)*rowHeight)/2
	for i := range tasks {
		y := top + i*rowHeight
		fillCircle(img, image.Rect(area.Min.X, y+(rowHeight-dot)/2, area.Min.X+dot, y+(rowHeight+dot)/2), glassesStateColor(tasks[i].State))
		text := fitGlassesText(face, fmt.Sprintf("%d %s", tasks[i].TaskNumber, tasks[i].Title), area.Max.X-textX)
		drawGlassesText(img, face, text, textX, y+baseline)
	}
	return img, nil
}

// glassesArea is the part of a width by height card of shape that shows text
// whole: the inscribed square of a circle, or the rectangle less a margin.
func glassesArea(shape gomode.DisplayShape, width, height int) image.Rectangle {
	bounds := image.Rect(0, 0, width, height)
	if shape == gomode.DisplayShapeCircle {
		side := min(width, height) * 707 / 1000
		center := image.Pt(width/2, height/2)
		return image.Rectangle{Min: center.Sub(image.Pt(side/2, side/2)), Max: center.Add(image.Pt(side/2, side/2))}
	}
	margin := min(width, height) / 25
	return bounds.Inset(margin)
}

// glassesStateColor shows what a task needs at a glance: amber when it waits
// for the user, green while it works, red when it failed, and grey when it
// stopped.
func glassesStateColor(state v1.TaskState) color.RGBA {
	switch state {
	case v1.TaskStateWaiting, v1.TaskStateAsking, v1.TaskStateHasPlan:
		return color.RGBA{R: 0xff, G: 0xb0, A: 0xff}
	case v1.TaskStatePending, v1.TaskStateBranching, v1.TaskStateProvisioning, v1.TaskStateStarting, v1.TaskStateRunning:
		return color.RGBA{R: 0x33, G: 0xcc, B: 0x33, A: 0xff}
	case v1.TaskStateFailed, v1.TaskStateCrashed:
		return color.RGBA{R: 0xe0, G: 0x3c, B: 0x31, A: 0xff}
	default:
		// Stopping, stopped, purging, and purged.
		return color.RGBA{R: 0x80, G: 0x80, B: 0x80, A: 0xff}
	}
}

// fitGlassesText cuts text at a rune and ends it with an ellipsis when it is
// wider than width pixels.
func fitGlassesText(face font.Face, text string, width int) string {
	limit := fixed.I(width)
	if font.MeasureString(face, text) <= limit {
		return text
	}
	runes := []rune(text)
	for n := len(runes) - 1; n > 0; n-- {
		if cut := string(runes[:n]) + "…"; font.MeasureString(face, cut) <= limit {
			return cut
		}
	}
	return "…"
}

func drawGlassesText(img *image.RGBA, face font.Face, text string, x, baseline int) {
	(&font.Drawer{Dst: img, Src: image.White, Face: face, Dot: fixed.P(x, baseline)}).DrawString(text)
}

// fillCircle fills the circle inscribed in r with c.
func fillCircle(img *image.RGBA, r image.Rectangle, c color.RGBA) {
	cx, cy := r.Min.X+r.Max.X, r.Min.Y+r.Max.Y
	radius := r.Dx()
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			// Doubled coordinates of the pixel centre keep the test in integers.
			if dx, dy := 2*x+1-cx, 2*y+1-cy; dx*dx+dy*dy <= radius*radius {
				img.SetRGBA(x, y, c)
			}
		}
	}
}
