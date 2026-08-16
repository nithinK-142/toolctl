# toolctl toolbox image: everything the CLI shells out to, bundled
# together since these tools are always invoked in sequence against
# the same mounted folder, never as separate long-running services.
FROM python:3.12-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        ffmpeg \
        jq \
        ca-certificates \
        curl \
    && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir yt-dlp gallery-dl

WORKDIR /data

ENTRYPOINT []
