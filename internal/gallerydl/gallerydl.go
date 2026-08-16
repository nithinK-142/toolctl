// Package gallerydl runs gallery-dl inside the toolbox container.
package gallerydl

import (
	"context"
	"os"
	"path/filepath"

	"github.com/nithinK-142/toolctl/internal/docker"
)

// Download runs gallery-dl against a URL. A config file at
// <mount>/gallery-dl.conf is loaded explicitly when present.
func Download(ctx context.Context, c *docker.Client, image, hostMount, url string) error {
	cmd := []string{"gallery-dl", "--destination", "/data", url}
	configPath := filepath.Join(hostMount, "gallery-dl.conf")
	if _, err := os.Stat(configPath); err == nil {
		cmd = []string{"gallery-dl", "--config", "/data/gallery-dl.conf", "--destination", "/data", url}
	}
	return c.Run(ctx, docker.RunOptions{
		Image:         image,
		HostMountPath: hostMount,
		Cmd:           cmd,
	})
}
