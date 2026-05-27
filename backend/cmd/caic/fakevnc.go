//go:build e2e

// Minimal VNC (RFB) server that serves a fake IDE screenshot for e2e
// documentation. Generates the image in-memory — no external dependencies.

package main

import (
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"io"
	"net"
)

// fakeVNCServer listens on a random localhost port and serves a single
// generated screenshot to every connecting VNC client.
type fakeVNCServer struct {
	ln   net.Listener
	port int
	img  []byte // raw RGBA pixel data
	w, h int
	done chan struct{}
}

// startFakeVNC creates the fake VNC server and starts accepting connections.
func startFakeVNC(ctx context.Context) (*fakeVNCServer, error) {
	img := generateFakeScreenshot()
	w, h := img.Rect.Dx(), img.Rect.Dy()
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	fv := &fakeVNCServer{
		ln:   ln,
		port: ln.Addr().(*net.TCPAddr).Port,
		img:  img.Pix,
		w:    w,
		h:    h,
		done: make(chan struct{}),
	}
	go fv.serve()
	return fv, nil
}

// Port returns the TCP port the server is listening on.
func (fv *fakeVNCServer) Port() int { return fv.port }

// Close stops the VNC listener.
func (fv *fakeVNCServer) Close() error {
	close(fv.done)
	return fv.ln.Close()
}

func (fv *fakeVNCServer) serve() {
	for {
		conn, err := fv.ln.Accept()
		if err != nil {
			return
		}
		go fv.handle(conn)
	}
}

// handle runs the RFB protocol handshake and serves the screenshot.
// Implements RFB 3.8 with no authentication and raw encoding.
func (fv *fakeVNCServer) handle(conn net.Conn) {
	defer conn.Close()

	buf := make([]byte, 256)

	// ProtocolVersion: server → client.
	conn.Write([]byte("RFB 003.008\n"))
	if _, err := io.ReadFull(conn, buf[:12]); err != nil {
		return
	}

	// Security: offer "None" (type 1).
	conn.Write([]byte{1, 1}) // number-of-types=1, type=1 (None)
	if _, err := io.ReadFull(conn, buf[:1]); err != nil {
		return
	}
	// SecurityResult: OK.
	conn.Write([]byte{0, 0, 0, 0})

	// ClientInit: shared-flag (ignored).
	if _, err := io.ReadFull(conn, buf[:1]); err != nil {
		return
	}

	// ServerInit: framebuffer dimensions + pixel format + name.
	var si []byte
	si = binary.BigEndian.AppendUint16(si, uint16(fv.w))
	si = binary.BigEndian.AppendUint16(si, uint16(fv.h))
	// Pixel format (16 bytes): 32-bit RGB, little-endian, red/green/blue at shifts 0/8/16.
	// This matches Go's image.RGBA.Pix layout: [R, G, B, A, ...].
	pf := []byte{
		32,     // bits-per-pixel
		24,     // depth
		0,      // big-endian-flag
		1,      // true-colour-flag
		0, 255, // red-max
		0, 255, // green-max
		0, 255, // blue-max
		16,      // red-shift
		8,       // green-shift
		0,       // blue-shift
		0, 0, 0, // padding
	}
	si = append(si, pf...)
	name := "caic-e2e"
	si = binary.BigEndian.AppendUint32(si, uint32(len(name)))
	si = append(si, []byte(name)...)
	conn.Write(si)

	// Message loop: handle client requests.
	for {
		var msgType [1]byte
		if _, err := io.ReadFull(conn, msgType[:]); err != nil {
			return
		}
		switch msgType[0] {
		case 0: // SetPixelFormat (19 bytes after type) — ignore.
			_, _ = io.ReadFull(conn, buf[:19])
		case 2: // SetEncodings: 1 padding + 2 count + n*4 encodings.
			if _, err := io.ReadFull(conn, buf[:3]); err != nil {
				return
			}
			n := int(binary.BigEndian.Uint16(buf[1:3]))
			if _, err := io.ReadFull(conn, make([]byte, n*4)); err != nil {
				return
			}
		case 3: // FramebufferUpdateRequest.
			if _, err := io.ReadFull(conn, buf[:9]); err != nil {
				return
			}
			incremental := buf[0]
			if incremental == 0 {
				fv.sendUpdate(conn)
			} else {
				// No changes — send zero-rectangle update.
				conn.Write([]byte{0, 0, 0, 0})
			}
		case 4: // KeyEvent (7 bytes after type) — ignore.
			_, _ = io.ReadFull(conn, buf[:7])
		case 5: // PointerEvent (5 bytes after type) — ignore.
			_, _ = io.ReadFull(conn, buf[:5])
		case 6: // ClientCutText: 3 padding + 4 length + text.
			if _, err := io.ReadFull(conn, buf[:7]); err != nil {
				return
			}
			n := int(binary.BigEndian.Uint32(buf[3:7]))
			if _, err := io.ReadFull(conn, make([]byte, n)); err != nil {
				return
			}
		default:
			return // unknown message type — close connection.
		}
	}
}

// sendUpdate writes a FramebufferUpdate with one full-screen raw rectangle.
func (fv *fakeVNCServer) sendUpdate(conn net.Conn) {
	var hdr []byte
	hdr = append(hdr, 0)                        // message-type = 0 (FramebufferUpdate)
	hdr = append(hdr, 0)                        // padding
	hdr = binary.BigEndian.AppendUint16(hdr, 1) // number of rectangles
	// Rectangle: x, y, w, h, encoding-type (0 = raw).
	hdr = binary.BigEndian.AppendUint16(hdr, 0)
	hdr = binary.BigEndian.AppendUint16(hdr, 0)
	hdr = binary.BigEndian.AppendUint16(hdr, uint16(fv.w))
	hdr = binary.BigEndian.AppendUint16(hdr, uint16(fv.h))
	hdr = append(hdr, 0, 0, 0, 0) // raw encoding
	conn.Write(hdr)
	conn.Write(fv.img)
}

// ── Screenshot generation ──────────────────────────────────────────────

// Colours used in the fake IDE screenshot.
var (
	cBG          = color.RGBA{30, 30, 30, 255}    // #1e1e1e editor background
	cTitleBG     = color.RGBA{50, 50, 60, 255}    // title bar
	cSidebarBG   = color.RGBA{37, 37, 38, 255}    // sidebar
	cStatusBG    = color.RGBA{0, 122, 204, 255}   // status bar (blue)
	cBorder      = color.RGBA{60, 60, 70, 255}    // separator
	cWhite       = color.RGBA{212, 212, 212, 255} // text
	cKeyword     = color.RGBA{86, 156, 214, 255}  // blue keyword
	cString      = color.RGBA{206, 145, 120, 255} // orange string
	cComment     = color.RGBA{106, 153, 85, 255}  // green comment
	cFunc        = color.RGBA{220, 220, 170, 255} // yellow function
	cType        = color.RGBA{78, 201, 176, 255}  // teal type
	cCursor      = color.RGBA{255, 255, 255, 255} // cursor
	cActiveTab   = color.RGBA{30, 30, 30, 255}    // active tab bg
	cInactiveTab = color.RGBA{45, 45, 50, 255}    // inactive tab bg
)

func generateFakeScreenshot() *image.RGBA {
	const w, h = 960, 600
	img := image.NewRGBA(image.Rect(0, 0, w, h))

	// Background.
	draw.Draw(img, img.Bounds(), image.NewUniform(cBG), image.Point{}, draw.Src)

	// ── Title bar ──
	fillRect(img, 0, 0, w, 36, cTitleBG)
	// Window title text (colored bars)
	drawTextBar(img, 12, 10, 200, 18, cWhite)

	// Tabs
	fillRect(img, 0, 36, w, 32, color.RGBA{37, 37, 38, 255})
	fillRect(img, 0, 36, 160, 32, cActiveTab) // active tab
	fillRect(img, 160, 36, 140, 32, cInactiveTab)
	fillRect(img, 300, 36, 140, 32, cInactiveTab)
	// Tab labels
	drawTextBar(img, 12, 44, 140, 16, cWhite)

	// ── Sidebar ──
	sbW := 200
	fillRect(img, 0, 68, sbW, h-68-24, cSidebarBG)
	fillRect(img, sbW, 68, 1, h-68-24, cBorder) // separator

	// Sidebar items (folder + file icons as small colored rectangles)
	sidebarY := 78
	for _, item := range []struct {
		textW int
	}{
		{130},
		{90},
		{70},
		{110},
		{80},
		{100},
		{60},
		{85},
	} {
		drawTextBar(img, 20, sidebarY, item.textW, 14, cWhite)
		sidebarY += 22
	}

	// ── Editor area ──
	editorX := sbW + 1 // 201
	editorY := 68

	// Line numbers gutter
	gutterW := 48
	fillRect(img, editorX, editorY, gutterW, h-68-24, color.RGBA{30, 30, 30, 255})
	fillRect(img, editorX+gutterW, editorY, 1, h-68-24, color.RGBA{50, 50, 55, 255})

	// Line numbers
	for i := range 20 {
		yy := editorY + 12 + i*20
		drawTextBar(img, editorX+8, yy, 30, 14, color.RGBA{100, 100, 105, 255})
	}

	// Code lines — each is a row of colored bars simulating syntax-highlighted text.
	type token struct {
		w int
		c color.RGBA
	}
	lines := [][]token{
		// 1  package main
		{{45, cKeyword}, {30, cWhite}, {90, cType}},
		// 2  (blank)
		{},
		// 3  import (
		{{50, cKeyword}},
		// 4      "fmt"
		{{30, cWhite}, {40, cString}},
		// 5      "os"
		{{30, cWhite}, {40, cString}},
		// 6      "strings"
		{{30, cWhite}, {40, cString}},
		// 7  )
		{{10, cKeyword}},
		// 8  (blank)
		{},
		// 9  // BuildTask creates and returns a new BuildTask.
		{{70, cComment}},
		// 10 func buildTask(name string, opts ...Option) *Task {
		{{30, cKeyword}, {50, cFunc}, {20, cWhite}, {30, cType}, {20, cWhite}, {20, cKeyword}, {60, cType}, {30, cWhite}},
		// 11     task := &Task{Name: name}
		{{40, cWhite}, {20, cKeyword}, {10, cWhite}, {30, cType}, {20, cWhite}, {20, cWhite}},
		// 12 (blank)
		{},
		// 13     for _, opt := range opts {
		{{30, cKeyword}, {20, cWhite}, {30, cKeyword}, {20, cWhite}, {30, cKeyword}},
		// 14         opt(task)
		{{40, cWhite}, {10, cWhite}},
		// 15     }
		{{20, cWhite}},
		// 16     return task
		{{50, cKeyword}, {30, cWhite}},
		// 17 }
		{{10, cWhite}},
	}

	codeX := editorX + gutterW + 1
	for i, tokens := range lines {
		yy := editorY + 12 + i*20
		x := codeX + 8
		for _, t := range tokens {
			drawTextBar(img, x, yy, t.w, 14, t.c)
			x += t.w + 6
		}
	}

	// Cursor on line 11
	cursorLine := 10 // 0-indexed
	cursorY := editorY + 12 + cursorLine*20
	cursorX := codeX + 8 + 40 + 6 + 20 + 6 + 10 + 6 // after "task := &Task{Name: name"
	// approximate
	cursorX = codeX + 8 + 40 + 6 + 20 + 6 + 10 + 6 + 30 + 6 + 20 + 3
	fillRect(img, cursorX, cursorY-1, 2, 17, cCursor)

	// ── Status bar ──
	sbY := h - 24
	fillRect(img, 0, sbY, w, 24, cStatusBG)
	drawTextBar(img, 10, sbY+5, 180, 14, color.RGBA{255, 255, 255, 255})
	drawTextBar(img, w-80, sbY+5, 70, 14, color.RGBA{200, 220, 240, 255})

	return img
}

// fillRect fills a rectangle with a solid colour.
func fillRect(img *image.RGBA, x, y, w, h int, c color.RGBA) {
	draw.Draw(img, image.Rect(x, y, x+w, y+h), image.NewUniform(c), image.Point{}, draw.Src)
}

// drawTextBar draws a horizontal coloured bar to simulate a line of text.
func drawTextBar(img *image.RGBA, x, y, w, h int, c color.RGBA) {
	fillRect(img, x, y, w, h, c)
}
