// Package ytdlp orchestrates yt-dlp runs inside the toolbox container.
// The container only ever runs one yt-dlp invocation at a time and
// knows nothing about prior runs — all "brains" (deciding whether to
// resume from a cached *.info.json, which format to reuse, whether
// subtitles need a fresh fetch) live here in Go, reading and writing
// the mounted host folder directly.
package ytdlp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nithinK-142/toolctl/internal/docker"
	"github.com/nithinK-142/toolctl/internal/mux"
)

// VideoOptions configures the full download+mux pipeline.
type VideoOptions struct {
	Subtitles bool
	Chapters  bool
	Quality   string // "1080", "720", "480", or "best"
	SubLang   string // defaults to "en"
}

var videoIDPattern = regexp.MustCompile(`(?:v=|youtu\.be/)([\w-]{11})`)

func videoID(url string) string {
	m := videoIDPattern.FindStringSubmatch(url)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func sanitizeFilename(name string) string {
	re := regexp.MustCompile(`[<>:"/\\|?*\s\x{00a0}]+`)
	return strings.TrimSpace(re.ReplaceAllString(name, " "))
}

// Audio runs `yt-dlp -x --audio-format mp3 --audio-quality 0 <url>`,
// writing output into the mounted data folder.
func Audio(ctx context.Context, c *docker.Client, image, hostMount, url string) error {
	return c.Run(ctx, docker.RunOptions{
		Image:         image,
		HostMountPath: hostMount,
		Cmd: []string{
			"yt-dlp",
			"-x", "--audio-format", "mp3", "--audio-quality", "0",
			"-o", "%(title)s.%(ext)s",
			url,
		},
	})
}

// Video runs the full video + chapters + subtitles pipeline: reuse a
// cached *.info.json when one exists for this video id (skipping the
// extra metadata/subtitle extraction calls that tend to trip yt-dlp
// rate limits), otherwise do a single fresh download that also writes
// the info.json for next time. Chapters and subtitles are muxed into a
// final .mkv via the mux package.
func Video(ctx context.Context, c *docker.Client, image, hostMount, url string, opts VideoOptions) error {
	if opts.SubLang == "" {
		opts.SubLang = "en"
	}
	if opts.Quality == "" {
		opts.Quality = "720"
	}

	id := videoID(url)
	cachedPath, meta := findCachedInfoJSON(hostMount, id)

	if meta != nil {
		fmt.Fprintf(os.Stderr, "Found cached metadata (%s) — resuming video only.\n", filepath.Base(cachedPath))
		return videoFromCache(ctx, c, image, hostMount, url, meta, opts)
	}

	fmt.Fprintln(os.Stderr, "No cached metadata — running a fresh download (video + info.json in one call).")
	return videoFresh(ctx, c, image, hostMount, url, opts)
}

// findCachedInfoJSON scans hostMount for a *.info.json whose "id" field
// matches videoID. Reading happens directly on the host filesystem —
// the mounted folder is just a normal directory to the Go process.
func findCachedInfoJSON(hostMount, id string) (string, map[string]any) {
	if id == "" {
		return "", nil
	}
	matches, _ := filepath.Glob(filepath.Join(hostMount, "*.info.json"))
	for _, p := range matches {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var meta map[string]any
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}
		if metaID, _ := meta["id"].(string); metaID == id {
			return p, meta
		}
	}
	return "", nil
}

func formatString(quality string) string {
	if quality == "best" {
		return "bestvideo+bestaudio/best"
	}
	return fmt.Sprintf("bestvideo[height<=%s]+bestaudio/best[height<=%s]", quality, quality)
}

// videoFresh performs the original single-call flow: one yt-dlp
// invocation handles video, info.json, and (optionally) subtitles.
func videoFresh(ctx context.Context, c *docker.Client, image, hostMount, url string, opts VideoOptions) error {
	cmd := []string{
		"yt-dlp",
		"-o", "%(title)s.%(ext)s",
		"-f", formatString(opts.Quality),
		"--merge-output-format", "mkv",
		"--write-info-json",
		"--continue",
		"--no-warnings",
	}
	if opts.Subtitles {
		cmd = append(cmd,
			"--write-sub", "--write-auto-sub",
			"--sub-lang", opts.SubLang,
			"--convert-subs", "srt",
		)
	}
	cmd = append(cmd, url)

	if err := c.Run(ctx, docker.RunOptions{Image: image, HostMountPath: hostMount, Cmd: cmd}); err != nil {
		return err
	}

	id := videoID(url)
	_, meta := findCachedInfoJSON(hostMount, id)
	if meta == nil {
		return fmt.Errorf("download finished but couldn't locate the resulting info.json")
	}
	return muxIfNeeded(ctx, c, image, hostMount, meta, opts)
}

// videoFromCache resumes the video download only (no redundant
// metadata/subtitle calls) and reuses chapters + subtitle URL already
// present in the cached info.json.
func videoFromCache(ctx context.Context, c *docker.Client, image, hostMount, url string, meta map[string]any, opts VideoOptions) error {
	cmd := []string{
		"yt-dlp",
		"-o", "%(title)s.%(ext)s",
		"-f", formatString(opts.Quality),
		"--merge-output-format", "mkv",
		"--continue",
		"--no-write-info-json",
		"--no-write-sub", "--no-write-auto-sub",
		"--no-warnings",
	}
	if opts.Subtitles {
		// Cached subtitle URLs expire; simplest reliable path is one
		// small subs-only fetch alongside the resumed video download.
		cmd = append(cmd, "--write-sub", "--write-auto-sub", "--sub-lang", opts.SubLang, "--convert-subs", "srt")
	}
	cmd = append(cmd, url)

	if err := c.Run(ctx, docker.RunOptions{Image: image, HostMountPath: hostMount, Cmd: cmd}); err != nil {
		return err
	}
	return muxIfNeeded(ctx, c, image, hostMount, meta, opts)
}

// muxIfNeeded writes a chapters metadata file (pure Go, no container
// needed) and, if chapters or subtitles are wanted, calls mux.Combine
// to run ffmpeg inside the toolbox container.
func muxIfNeeded(ctx context.Context, c *docker.Client, image, hostMount string, meta map[string]any, opts VideoOptions) error {
	title, _ := meta["title"].(string)
	base := sanitizeFilename(title)
	if base == "" {
		base = "video"
	}

	videoFile := findDownloadedFile(hostMount, base)
	if videoFile == "" {
		return fmt.Errorf("couldn't locate downloaded video file for %q", base)
	}

	var chaptersRel string
	if opts.Chapters {
		chapters, _ := meta["chapters"].([]any)
		duration, _ := meta["duration"].(float64)
		if len(chapters) > 0 {
			chaptersRel = base + ".chapters.txt"
			if err := mux.WriteChaptersFile(filepath.Join(hostMount, chaptersRel), chapters, duration); err != nil {
				return err
			}
		}
	}

	var subsRel string
	if opts.Subtitles {
		subsRel = findSubtitleFile(hostMount, base, opts.SubLang)
	}

	if chaptersRel == "" && subsRel == "" {
		fmt.Fprintln(os.Stderr, "No subs/chapters to embed — video saved as-is.")
		return nil
	}

	return mux.Combine(ctx, c, image, hostMount, mux.CombineOptions{
		VideoRel:    filepath.Base(videoFile),
		SubsRel:     subsRel,
		ChaptersRel: chaptersRel,
		FinalRel:    base + " [final].mkv",
	})
}

func findDownloadedFile(hostMount, base string) string {
	for _, ext := range []string{"mkv", "mp4", "webm"} {
		p := filepath.Join(hostMount, base+"."+ext)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func findSubtitleFile(hostMount, base, lang string) string {
	matches, _ := filepath.Glob(filepath.Join(hostMount, base+"."+lang+"*.srt"))
	if len(matches) == 0 {
		return ""
	}
	return filepath.Base(matches[0])
}
