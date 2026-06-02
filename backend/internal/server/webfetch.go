// HTTP handler for POST /api/v1/web/fetch: fetches a URL and extracts text content.

package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	v1 "github.com/caic-xyz/caic/backend/internal/api/v1"
)

const (
	webFetchTimeout    = 15 * time.Second
	webFetchMaxContent = 8000
	webFetchMaxBody    = 2 << 20 // 2 MiB
)

func (s *Server) webFetch(ctx context.Context, req *v1.WebFetchReq) (*v1.WebFetchResp, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (compatible; caic/1.0)")

	client := &http.Client{Timeout: webFetchTimeout}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("fetching URL: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, req.URL)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, webFetchMaxBody))
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	title, content := extractHTML(body)
	return &v1.WebFetchResp{Title: title, Content: content}, nil
}

// skipTags are elements whose text content should be discarded.
var skipTags = map[atom.Atom]struct{}{
	atom.Script:   {},
	atom.Style:    {},
	atom.Nav:      {},
	atom.Noscript: {},
}

var collapseWS = regexp.MustCompile(`[ \t]+`)

// extractHTML parses raw HTML bytes and returns the <title> and body text.
func extractHTML(data []byte) (title, content string) {
	doc, err := html.Parse(strings.NewReader(string(data)))
	if err != nil {
		return "", string(data)
	}

	var titleBuf strings.Builder
	var bodyBuf strings.Builder
	var inTitle bool

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if _, ok := skipTags[n.DataAtom]; ok {
				return
			}
			if n.DataAtom == atom.Title {
				inTitle = true
				defer func() { inTitle = false }()
			}
		}
		if n.Type == html.TextNode {
			text := strings.TrimSpace(n.Data)
			if text != "" {
				if inTitle {
					titleBuf.WriteString(text)
				}
				bodyBuf.WriteString(text)
				bodyBuf.WriteByte(' ')
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	content = collapseWS.ReplaceAllString(bodyBuf.String(), " ")
	content = strings.TrimSpace(content)
	if len(content) > webFetchMaxContent {
		content = content[:webFetchMaxContent]
	}
	return strings.TrimSpace(titleBuf.String()), content
}
