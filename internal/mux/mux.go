// Package mux handles combining a downloaded video with subtitles and
// chapter markers into a single .mkv. Writing the ffmpeg chapters
// metadata file is pure Go (no container needed, since the mounted
// folder is a normal directory on the host); the actual mux is a
// single ffmpeg call inside the toolbox container.
package mux

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/nithinK-142/toolctl/internal/docker"
)

// WriteChaptersFile writes an ffmpeg ;FFMETADATA1 file describing
// chapter markers, given yt-dlp's "chapters" list from an info.json
// (each entry has start_time, optionally end_time, and title).
func WriteChaptersFile(path string, chapters []any, duration float64) error {
	var b strings.Builder
	b.WriteString(";FFMETADATA1\n")

	for i, raw := range chapters {
		ch, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		start, _ := ch["start_time"].(float64)
		end, ok := ch["end_time"].(float64)
		if !ok {
			end = duration
		}
		title, _ := ch["title"].(string)
		if title == "" {
			title = fmt.Sprintf("Chapter %d", i+1)
		}
		title = escapeFFMetadata(title)
		fmt.Fprintf(&b, "[CHAPTER]\nTIMEBASE=1/1000\nSTART=%d\nEND=%d\ntitle=%s\n",
			int64(start*1000), int64(end*1000), title)
	}

	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func escapeFFMetadata(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "=", "\\=")
	s = strings.ReplaceAll(s, ";", "\\;")
	s = strings.ReplaceAll(s, "#", "\\#")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", "\\\n")
	return s
}

// CombineOptions describes a single mux call. All paths are relative
// to the mounted data folder (i.e. as they'd appear under /data in
// the container).
type CombineOptions struct {
	VideoRel      string // required
	SubsRel       string // optional
	ChaptersRel   string // optional
	FinalRel      string // output filename
	Title         string // output container title
	SubtitleLang  string // subtitle language metadata
	SubtitleTitle string // subtitle track title
}

// Combine runs one ffmpeg invocation inside the toolbox container to
// mux video + optional subtitle track + optional chapter markers into
// FinalRel, using stream copy (no re-encoding).
func Combine(ctx context.Context, c *docker.Client, image, hostMount string, opts CombineOptions) error {
	if opts.VideoRel == "" || opts.FinalRel == "" {
		return fmt.Errorf("video and final output paths are required")
	}

	cmd := []string{"ffmpeg", "-hide_banner", "-y", "-i", opts.VideoRel}
	inputIdx := 1
	subInputIdx := -1
	chaptersInputIdx := -1

	if opts.SubsRel != "" {
		cmd = append(cmd, "-i", opts.SubsRel)
		subInputIdx = inputIdx
		inputIdx++
	}
	if opts.ChaptersRel != "" {
		cmd = append(cmd, "-f", "ffmetadata", "-i", opts.ChaptersRel)
		chaptersInputIdx = inputIdx
	}

	cmd = append(cmd, "-map", "0:v:0")
	cmd = append(cmd, "-map", "0:a:0?")
	if subInputIdx >= 0 {
		cmd = append(cmd, "-map", fmt.Sprintf("%d:s:0", subInputIdx))
	}
	if chaptersInputIdx >= 0 {
		cmd = append(cmd, "-map_chapters", fmt.Sprintf("%d", chaptersInputIdx))
	}

	cmd = append(cmd, "-c:v", "copy", "-c:a", "copy")
	if opts.Title != "" {
		cmd = append(cmd, "-metadata", "title="+opts.Title)
	}
	if subInputIdx >= 0 {
		cmd = append(cmd, "-c:s", "subrip")
		if opts.SubtitleLang != "" {
			cmd = append(cmd, "-metadata:s:s:0", "language="+opts.SubtitleLang)
		}
		if opts.SubtitleTitle != "" {
			cmd = append(cmd, "-metadata:s:s:0", "title="+opts.SubtitleTitle)
		}
		cmd = append(cmd, "-disposition:s:0", "default")
	}
	cmd = append(cmd, opts.FinalRel)

	fmt.Fprintf(os.Stderr, "Muxing final file -> %s\n", opts.FinalRel)
	if err := c.Run(ctx, docker.RunOptions{Image: image, HostMountPath: hostMount, Cmd: cmd}); err != nil {
		return fmt.Errorf("ffmpeg mux failed: %w", err)
	}
	return nil
}
