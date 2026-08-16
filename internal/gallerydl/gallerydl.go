// Package gallerydl runs gallery-dl inside the toolbox container.
package gallerydl

import (
	"context"

	"github.com/nithinK-142/toolctl/internal/docker"
)

// Download runs `gallery-dl <url>` with output rooted at the mounted
// data folder. gallery-dl's own config file (cookies, extractor
// options, etc.) is expected at <mount>/gallery-dl.conf, which the
// tool picks up automatically when run with --data-directory /data.
func Download(ctx context.Context, c *docker.Client, image, hostMount, url string) error {
	return c.Run(ctx, docker.RunOptions{
		Image:         image,
		HostMountPath: hostMount,
		Cmd:           []string{"gallery-dl", "-d", ".", url},
	})
}
