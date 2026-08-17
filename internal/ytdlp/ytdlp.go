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
	"unicode/utf8"

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

var videoIDPattern = regexp.MustCompile(`(?:[?&]v=|youtu\.be/|youtube\.com/shorts/)([\w-]{11})`)

func videoID(url string) string {
	m := videoIDPattern.FindStringSubmatch(url)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func validateQuality(q string) error {
	if q == "best" {
		return nil
	}
	switch q {
	case "480", "720", "1080":
		return nil
	default:
		return fmt.Errorf("invalid quality %q: use 480, 720, 1080, or best", q)
	}
}

func outputBase(meta map[string]any, fallbackID string) string {
	title, _ := meta["title"].(string)
	title = sanitizeFilename(title)
	if title != "" {
		return title
	}
	if fallbackID != "" {
		return fallbackID
	}
	return "video"
}

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		switch r {
		case '\x00', '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '-'
		default:
			return r
		}
	}, name)
	name = strings.Trim(name, " .")
	if !utf8.ValidString(name) {
		name = fallbackASCII(name)
	}
	if name == "" {
		return "video"
	}
	const maxNameBytes = 180
	if len(name) > maxNameBytes {
		for len(name) > maxNameBytes {
			_, size := utf8.DecodeLastRuneInString(name)
			if size == 0 {
				break
			}
			name = name[:len(name)-size]
		}
		name = strings.TrimSpace(name)
	}
	return name
}

func fallbackASCII(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 128 && r >= 32 {
			b.WriteRune(r)
		}
	}
	return b.String()
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
	if err := validateQuality(opts.Quality); err != nil {
		return err
	}
	if strings.TrimSpace(url) == "" {
		return fmt.Errorf("URL cannot be empty")
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
func findLatestInfoJSON(hostMount string) (string, map[string]any) {
	matches, _ := filepath.Glob(filepath.Join(hostMount, "*.info.json"))
	var latestPath string
	var latestTime int64
	for _, p := range matches {
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		mtime := info.ModTime().UnixNano()
		if latestPath == "" || mtime > latestTime {
			latestPath = p
			latestTime = mtime
		}
	}
	if latestPath == "" {
		return "", nil
	}
	data, err := os.ReadFile(latestPath)
	if err != nil {
		return "", nil
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", nil
	}
	return latestPath, meta
}

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
		"-o", "%(id)s.%(ext)s",
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
		_, meta = findLatestInfoJSON(hostMount)
	}
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
		"-o", "%(id)s.%(ext)s",
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
	id := videoIDFromMeta(meta)
	base := id
	if base == "" {
		base = outputBase(meta, "")
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
		if subsRel == "" {
			return fmt.Errorf("subtitles requested but no %s subtitle file was produced", opts.SubLang)
		}
	}

	title, _ := meta["title"].(string)
	title = sanitizeFilename(title)
	if title == "" {
		title = base
	}

	if chaptersRel == "" && subsRel == "" {
		finalTarget := filepath.Join(hostMount, title+".mkv")
		if filepath.Clean(videoFile) != filepath.Clean(finalTarget) {
			if err := os.Rename(videoFile, finalTarget); err != nil {
				return fmt.Errorf("renaming video to title: %w", err)
			}
		}
		fmt.Fprintf(os.Stderr, "Done: %s\n", finalTarget)
		return nil
	}

	finalRel := base + " [final].mkv"

	if err := mux.Combine(ctx, c, image, hostMount, mux.CombineOptions{
		VideoRel:      filepath.Base(videoFile),
		SubsRel:       subsRel,
		ChaptersRel:   chaptersRel,
		FinalRel:      finalRel,
		Title:         title,
		SubtitleLang:  opts.SubLang,
		SubtitleTitle: opts.SubLang,
	}); err != nil {
		return err
	}

	finalPath := filepath.Join(hostMount, finalRel)
	finalTarget := filepath.Join(hostMount, title+".mkv")

	if filepath.Clean(videoFile) != filepath.Clean(finalTarget) {
		if err := os.Remove(videoFile); err != nil {
			return fmt.Errorf("removing pre-mux video %s: %w", filepath.Base(videoFile), err)
		}
	}
	if err := os.Rename(finalPath, finalTarget); err != nil {
		return fmt.Errorf("finalizing muxed video: %w", err)
	}
	if chaptersRel != "" {
		_ = os.Remove(filepath.Join(hostMount, chaptersRel))
	}
	if subsRel != "" {
		_ = os.Remove(filepath.Join(hostMount, subsRel))
	}

	fmt.Fprintf(os.Stderr, "Done: %s\n", finalTarget)
	return nil
}

func videoIDFromMeta(meta map[string]any) string {
	id, _ := meta["id"].(string)
	return id
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
