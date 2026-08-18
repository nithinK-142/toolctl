# toolctl

`toolctl` is a Go CLI for downloading media through disposable Docker containers.

It provides wrappers around:

- `yt-dlp` for YouTube/media downloads
- `gallery-dl` for galleries
- `ffmpeg` for media muxing

The Go binary contains the orchestration logic. The external tools run inside Docker.

## Requirements

- Go 1.26+
- Docker Engine 29+ or Docker Desktop
- Network access for image pulls/builds and media downloads

Runtime Docker communication uses the Moby Docker Engine client. `toolctl` does not call the `docker` CLI for normal operations.

## Build

From the project root:

```sh
go mod tidy
make build
make image
```

`make image` builds the toolbox image from the root `Dockerfile`:

```text
toolctl/tools:latest
```

The `docker` CLI is required for `make image`. After the image is built, normal `toolctl` operations communicate with Docker through the Engine API.

## Configure storage

Set the host directory used for persistent downloads and metadata:

```sh
./bin/toolctl config set-mount ~/Downloads/toolctl-data
```

The configured directory is bind-mounted into the Docker containers at `/data`.

## Commands

### Audio

```sh
./bin/toolctl audio "https://www.youtube.com/watch?v=DTFiB7DMeBk"
```

### Video

```sh
./bin/toolctl video "https://www.youtube.com/watch?v=gaqlIqbTfBs" --subs --chapters --quality 720
```

Flags may appear before or after the URL:

```sh
./bin/toolctl video --subs --chapters --quality 720 "https://www.youtube.com/watch?v=gaqlIqbTfBs"
```

### Gallery

```sh
./bin/toolctl gallery "https://example.com/gallery/123"
```

## YouTube PO-token support

Current YouTube requests can require Proof-of-Origin (PO) tokens. `toolctl` manages this automatically for YouTube audio/video downloads.

On the first YouTube audio/video operation, `toolctl`:

1. Pulls the pinned PO-token provider image if it is not local.
2. Creates a private Docker network named `toolctl-net`.
3. Starts the provider as `toolctl-bgutil`.
4. Waits for the provider's `/ping` endpoint to become available.
5. Discovers the provider's address from Docker instead of hardcoding a container IP.
6. Configures `yt-dlp` to use the `mweb` client and the BgUtils provider.

The provider does not expose a host port. It is reachable only from the Docker network used by the tool containers.

The pinned provider image is:

```text
brainicism/bgutil-ytdlp-pot-provider:1.3.1-deno
```

Deno is included in the toolbox for yt-dlp's current YouTube JavaScript challenge handling.

## Persistent data

The configured mount contains downloaded files and `.info.json` metadata used by the video resume flow.

Example:

```text
~/Downloads/toolctl-data/
├── <video title>.mkv
└── <video-id>.info.json
```

The `.info.json` file is intentionally retained so interrupted downloads can reuse cached metadata.

### gallery-dl configuration

An optional gallery-dl configuration file can be placed in the mount:

```text
<mount>/gallery-dl.conf
```

toolctl passes this configuration explicitly to gallery-dl when present.

## Toolbox versions

The toolbox currently pins:

| Tool | Version |
|---|---|
| yt-dlp | 2026.07.04 |
| gallery-dl | 1.32.8 |
| Deno | 2.9.4 |
| BgUtils PO-token provider | 1.3.1 |

FFmpeg, jq, curl, and CA certificates are installed from the Debian base image repositories.

## Docker Desktop and Docker Engine

The Go Docker client uses the Docker Engine API.

The same binary is intended to work with:

- Docker Desktop
- Docker Engine
- Docker contexts / configured Docker endpoints supported by the Engine client

The project does not require Docker Compose for runtime operation.

## Resume behavior

Video downloads keep metadata in `<video-id>.info.json`.

On a subsequent run, toolctl can reuse that metadata instead of performing the full metadata extraction again. Temporary playback URLs are not treated as permanent cache data.

## Troubleshooting

### No mount configured

Run:

```sh
./bin/toolctl config set-mount ~/Downloads/toolctl-data
```

### Toolbox image missing

Build it once:

```sh
make image
```

### PO-token provider problems

Check the provider container:

```sh
docker ps -a --filter "name=toolctl-bgutil"
```

Check its logs:

```sh
docker logs toolctl-bgutil
```

Check the provider endpoint from inside the provider container:

```sh
docker exec toolctl-bgutil \
  deno eval 'const r = await fetch("http://127.0.0.1:4416/ping"); console.log(r.status); console.log(await r.text())'
```

A working provider returns HTTP `200`.

### Rebuilding after code changes

Go code only:

```sh
gofmt -w internal/.../*.go
go mod tidy
make build
```

Dockerfile/toolbox changes:

```sh
make image
```

Do not rebuild the toolbox image for Go-only changes.

## Current limitations

- YouTube login, age restrictions, bot checks, or account-required content may require cookies.
- YouTube behavior can change independently of the project and yt-dlp releases.
- The first YouTube audio/video run may pull the PO-token provider image.
- The toolbox image must be rebuilt when pinned tool versions or bundled tools/plugins change.
