FROM denoland/deno:bin-2.9.4 AS deno

FROM python:3.12.13-slim-bookworm

COPY --from=deno /deno /usr/local/bin/deno

RUN apt-get update && apt-get install -y --no-install-recommends \
        ffmpeg \
        jq \
        ca-certificates \
        curl \
    && rm -rf /var/lib/apt/lists/*

RUN python -m pip install --no-cache-dir \
        "yt-dlp[default]==2026.7.4" \
        "gallery-dl==1.32.8" \
        "bgutil-ytdlp-pot-provider==1.3.1"

WORKDIR /data

ENTRYPOINT []
