# toolctl

A plug-and-play CLI for `yt-dlp`, `gallery-dl`, `ffmpeg`, and `jq`. The
Go binary has no runtime dependencies of its own — every tool
invocation runs inside a single ephemeral Docker container, with one
host folder bind-mounted in for config, cache, and output. Clone,
build the binary, point it at a folder, and go — nothing to `pip
install` or `apt install` on the host.

## Why this shape

**One toolbox image, not one per tool.** `yt-dlp`, `ffmpeg`, and
`gallery-dl` are invoked in *sequence* against the same mounted
folder — `yt-dlp` finishes, then `ffmpeg` runs on its output — never
concurrently. Splitting them into separate images is the right call
for actual long-running *services* with independent lifecycles (a web
server, a database); it buys nothing here and adds version-skew risk
between images. So there's exactly one image
(`docker/Dockerfile`) bundling all four tools, the same pattern used
by toolbox-style images like `linuxserver.io`'s media containers.

**The Go binary is the brain; the container is a dumb toolbox.** Every
container invocation is a single stateless command (`yt-dlp ...`,
`ffmpeg ...`). Decisions like "is there a cached `.info.json` for this
video, so we can resume instead of re-extracting metadata and
tripping a 429" live entirely in Go, which reads and writes the
mounted folder directly as a normal host directory — no extra
container calls needed just to inspect state.

**Docker Engine API client, not shelling out to the `docker` CLI.**
toolctl talks to the Docker daemon directly via
`github.com/moby/moby/client` — the maintained Engine API client
published by the moby/moby org (the successor to the now-deprecated
`github.com/docker/docker/client`, which carries unpatched advisories
as of this writing) — resolving the connection with `client.FromEnv`,
the same resolution logic the `docker` CLI itself uses (`DOCKER_HOST`,
Docker contexts, Docker Desktop's socket). This means toolctl works
identically whether you're running full Docker Desktop or just the
Engine + daemon, and doesn't require the `docker` binary to be on
`PATH` at all — only a running daemon.

```
Host machine                        Tools container (ephemeral)
┌─────────────────────┐             ┌─────────────────────────┐
│  toolctl (Go binary) │─ docker run ─▶│ yt-dlp, gallery-dl,     │
│  orchestrates, no    │             │ ffmpeg, jq               │
│  deps                │             └─────────────────────────┘
└─────────┬────────────┘                          │
          │                                        │
          └───────────────┬────────────────────────┘
                           ▼
                  Mounted folder (host)
                  config, cache, output
```

## Layout

```
cmd/toolctl/main.go     entrypoint, subcommand dispatch
internal/config/        ~/.config/toolctl/config.json (mount path, image tag)
internal/docker/        Docker SDK wrapper: EnsureImage, Run
internal/ytdlp/         audio + video pipelines, info.json caching logic
internal/mux/           chapters file + ffmpeg mux call
internal/gallerydl/     gallery-dl wrapper
docker/Dockerfile       the single toolbox image
Makefile                build binary / build image
```

## Setup

Requires: Go 1.26.5+ to build, and a running Docker daemon (Desktop or
Engine) to run.

```sh
git clone https://github.com/nithinK-142/toolctl.git
cd toolctl
make build                      # -> bin/toolctl
make image                      # builds toolctl/tools:latest locally
./bin/toolctl config set-mount ~/Downloads/toolctl-data
```

If you skip `make image`, the first tool command will auto-pull
`toolctl/tools:latest` if you've pushed it somewhere, or fail asking
you to build it locally — point `image` in
`~/.config/toolctl/config.json` at whatever tag you use.

## Usage

```sh
# audio only, mp3
toolctl audio "https://www.youtube.com/watch?v=DTFiB7DMeBk"

# full video with subtitles and chapters, muxed into one .mkv
toolctl video "https://www.youtube.com/watch?v=gaqlIqbTfBs" \
  --subs --chapters --quality 720

# gallery-dl against any supported site
toolctl gallery "https://example.com/gallery/123"
```

All output, `.info.json` caches, and any tool config files
(`gallery-dl.conf`, cookies, etc.) live under the mount path you set
with `config set-mount`. Re-running `video` on a URL you've already
started will find the cached `.info.json` there and resume the video
download only, instead of re-running metadata/subtitle extraction —
this is the same 429-avoidance behavior as the reference Python
script this was built from.

## Config file

`~/.config/toolctl/config.json`:

```json
{
  "mount_path": "/home/nithin/Downloads/toolctl-data",
  "image": "toolctl/tools:latest"
}
```

## Notes / next steps

- `go.mod` pins `github.com/moby/moby/client` and `.../api` by version;
  run `go mod tidy` right after cloning — these are young,
  independently-versioned modules (split out of Docker/Moby v29) and
  the pinned versions are a starting point, not guaranteed exact.
- The option-struct field names in `internal/docker/docker.go`
  (`ContainerCreateOptions`, `ContainerWaitOptions`/`Result`) are
  written from the client's current documented shape but haven't been
  compiled against a live copy of the module. If `go build` reports a
  field mismatch, run `go doc github.com/moby/moby/client
  ContainerCreateOptions` (and the Wait equivalents) to confirm the
  exact fields and adjust — the package doc comment in that file notes
  exactly where to look.
- `ffmpeg`/`yt-dlp`/`gallery-dl` flags are currently fixed inside each
  internal package — extending them (e.g. custom yt-dlp format
  strings, gallery-dl extractor options) means adding flags in
  `cmd/toolctl/main.go` and threading them through.
- No image versioning/pinning strategy yet beyond `latest` — worth
  tagging by tool versions once this is used seriously.
