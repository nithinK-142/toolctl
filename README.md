# toolctl

A Go CLI that runs `yt-dlp`, `gallery-dl`, and `ffmpeg` inside one disposable Docker toolbox. The host only needs Go to build and Docker Desktop/Engine to run.

## Requirements

- Go 1.26+
- Docker Engine 29+ or Docker Desktop
- Network access when building the toolbox image and downloading media

The project uses the supported post-Docker-v29 Moby Go client: `github.com/moby/moby/client`. Docker Engine API negotiation is automatic.

## Build

```sh
make build
make image
./bin/toolctl config set-mount ~/Downloads/toolctl-data
```

`make image` requires the Docker CLI because it builds the local toolbox image from the root `Dockerfile`. After that, normal `toolctl` operations use the Docker Engine API directly and do not invoke the Docker CLI.

## Commands

```sh
./bin/toolctl audio "https://www.youtube.com/watch?v=DTFiB7DMeBk"

./bin/toolctl video "https://www.youtube.com/watch?v=gaqlIqbTfBs" --subs --chapters --quality 720

./bin/toolctl video --subs --chapters --quality 720 "https://www.youtube.com/watch?v=gaqlIqbTfBs"

./bin/toolctl gallery "https://example.com/gallery/123"
```

Video flags can appear before or after the URL.

## Persistent data

The configured mount contains downloads and yt-dlp `.info.json` metadata used for resume logic. Optional gallery-dl configuration can be placed at:

```text
<mount>/gallery-dl.conf
```

When present, toolctl passes it explicitly to gallery-dl.

## Toolbox versions

The image pins the application versions used by the project: yt-dlp 2026.07.04, gallery-dl 1.32.8, and Deno 2.9.4. Deno is required for current YouTube JavaScript challenge solving; yt-dlp documents Deno as the recommended runtime. FFmpeg and jq come from the Debian base image repositories.

Current project references were checked against the current Moby client documentation and current upstream tool releases.

## Current limitations

- YouTube login/bot checks may still require cookies.
- The Go build cannot be validated in an offline environment; dependency resolution is performed by Go on the user's machine.
- The toolbox image should be rebuilt when its pinned tool versions are intentionally updated.
