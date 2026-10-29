// Package dlclient drives download_media from a shell: it calls the tool
// until the download is done, so an agent can run one command in the
// background and be told when it ends, instead of polling the file.
package dlclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// minCall is the least time between two calls. The server waits about
// 45 s per call; this only guards against a server that answers at once.
var minCall = time.Second

type status struct {
	Status     string `json:"status"`
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Downloaded int64  `json:"downloaded"`
	EtaSeconds int64  `json:"eta_seconds"`
}

// Run calls download_media with args until the file is done and returns
// its final path, writing one progress line per "downloading" answer. A
// failed download returns the tool's error. Stopping Run (ctx) stops only
// the waiting: the download is the server's and goes on.
func Run(ctx context.Context, cs *mcp.ClientSession, args map[string]any, progress io.Writer) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		start := time.Now()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "download_media", Arguments: args})
		if err != nil {
			return "", err
		}
		if res.IsError {
			return "", errors.New(text(res))
		}
		var st status
		b, err := json.Marshal(res.StructuredContent)
		if err == nil {
			err = json.Unmarshal(b, &st)
		}
		if err != nil {
			return "", fmt.Errorf("read download_media's answer: %w", err)
		}
		switch st.Status {
		case "done":
			return st.Path, nil
		case "downloading":
			fmt.Fprintf(progress, "downloading %s: %d of %d bytes (%d%%)%s\n",
				st.Path, st.Downloaded, st.Size, percent(st.Downloaded, st.Size), eta(st.EtaSeconds))
		default:
			return "", fmt.Errorf("unexpected download_media status %q", st.Status)
		}
		if wait := minCall - time.Since(start); wait > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(wait):
			}
		}
	}
}

func text(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, " ")
}

func percent(n, of int64) int64 {
	if of <= 0 {
		return 0
	}
	return n * 100 / of
}

func eta(seconds int64) string {
	if seconds <= 0 {
		return ""
	}
	return fmt.Sprintf(", about %s left", time.Duration(seconds)*time.Second)
}
