// toolctl is a plug-and-play CLI for yt-dlp, gallery-dl, ffmpeg, and
// jq. It has no runtime dependencies of its own — every tool
// invocation runs inside a single ephemeral Docker container, with a
// user-chosen host folder bind-mounted in for config, cache, and
// output. See README.md for the full design.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/nithinK-142/toolctl/internal/config"
	"github.com/nithinK-142/toolctl/internal/docker"
	"github.com/nithinK-142/toolctl/internal/gallerydl"
	"github.com/nithinK-142/toolctl/internal/ytdlp"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}

	switch args[0] {
	case "config":
		return runConfig(args[1:])
	case "audio":
		return runAudio(ctx, args[1:])
	case "video":
		return runVideo(ctx, args[1:])
	case "gallery":
		return runGallery(ctx, args[1:])
	case "-h", "--help", "help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, `toolctl — Docker-based yt-dlp / gallery-dl / ffmpeg toolbox

Usage:
  toolctl config set-mount <path>       Set the host folder used for config/output
  toolctl audio <url>                   Extract audio only (mp3)
  toolctl video <url> [flags]           Download video, optionally with subs/chapters
      --subs              Include subtitles
      --chapters          Embed chapters
      --quality <q>       1080|720|480|best (default: 720)
      --lang <code>       Subtitle language (default: en)
  toolctl gallery <url>                 Run gallery-dl against a URL

Config lives at ~/.config/toolctl/config.json
`)
}

func runConfig(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: toolctl config set-mount <path>")
	}
	switch args[0] {
	case "set-mount":
		if len(args) < 2 {
			return fmt.Errorf("usage: toolctl config set-mount <path>")
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		cfg.MountPath = args[1]
		if err := config.Save(cfg); err != nil {
			return err
		}
		fmt.Printf("Mount path set to %s\n", args[1])
		return nil
	default:
		return fmt.Errorf("unknown config subcommand %q", args[0])
	}
}

// dockerClient sets up a Docker SDK client, resolves config, and
// ensures the toolbox image is present — the shared setup every tool
// command needs before it can run a container.
func dockerClient(ctx context.Context, needsPOTProvider bool) (*docker.Client, *config.Config, error) {
	cfg, err := config.RequireMount()
	if err != nil {
		return nil, nil, err
	}
	c, err := docker.NewClient()
	if err != nil {
		return nil, nil, err
	}
	if err := c.EnsureImage(ctx, cfg.Image); err != nil {
		c.Close()
		return nil, nil, err
	}
	if needsPOTProvider {
		if err := c.EnsurePOTProvider(ctx, cfg.POTProviderImage); err != nil {
			c.Close()
			return nil, nil, err
		}
	}
	return c, cfg, nil
}

func runAudio(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: toolctl audio <url>")
	}
	c, cfg, err := dockerClient(ctx, true)
	if err != nil {
		return err
	}
	defer c.Close()
	return ytdlp.Audio(ctx, c, cfg.Image, cfg.MountPath, args[0])
}

func runVideo(ctx context.Context, args []string) error {
	flagArgs, url, err := splitVideoArgs(args)
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("video", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	subs := fs.Bool("subs", false, "include subtitles")
	chapters := fs.Bool("chapters", false, "embed chapters")
	quality := fs.String("quality", "720", "1080|720|480|best")
	lang := fs.String("lang", "en", "subtitle language")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	c, cfg, err := dockerClient(ctx, true)
	if err != nil {
		return err
	}
	defer c.Close()

	return ytdlp.Video(ctx, c, cfg.Image, cfg.MountPath, url, ytdlp.VideoOptions{
		Subtitles: *subs,
		Chapters:  *chapters,
		Quality:   *quality,
		SubLang:   *lang,
	})
}

func splitVideoArgs(args []string) (flagArgs []string, url string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--quality" || arg == "--lang" {
			flagArgs = append(flagArgs, arg)
			if i+1 >= len(args) {
				return nil, "", fmt.Errorf("%s requires a value", arg)
			}
			flagArgs = append(flagArgs, args[i+1])
			i++
			continue
		}
		if strings.HasPrefix(arg, "-") {
			flagArgs = append(flagArgs, arg)
			continue
		}
		if url != "" {
			return nil, "", fmt.Errorf("unexpected extra argument %q", arg)
		}
		url = arg
	}
	if url == "" {
		return nil, "", fmt.Errorf("usage: toolctl video <url> [--subs] [--chapters] [--quality Q] [--lang L]")
	}
	return flagArgs, url, nil
}

func runGallery(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: toolctl gallery <url>")
	}
	c, cfg, err := dockerClient(ctx, false)
	if err != nil {
		return err
	}
	defer c.Close()
	return gallerydl.Download(ctx, c, cfg.Image, cfg.MountPath, args[0])
}
