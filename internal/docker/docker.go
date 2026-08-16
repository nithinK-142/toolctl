// Package docker wraps the subset of the Docker Engine API client that
// toolctl needs: resolving a client the same way the docker CLI does
// (respecting DOCKER_HOST / contexts / Docker Desktop's socket), ensuring
// the toolbox image is present, and running one-shot ephemeral containers
// with a single bind mount.
//
// Built against github.com/moby/moby/client — the maintained successor to
// the now-deprecated github.com/docker/docker/client. See go.mod for why.
package docker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// ContainerDataDir is the fixed path inside every tool container that
// the host mount path is bound to.
const ContainerDataDir = "/data"

// Client wraps a resolved Docker Engine API client.
type Client struct {
	cli *client.Client
}

// NewClient resolves a Docker client from the environment exactly like
// the docker CLI does: DOCKER_HOST, docker contexts, and Docker
// Desktop's named-pipe/socket are all honored via FromEnv, and API
// version negotiation happens automatically on the first request. This
// means toolctl works identically whether the user has only the Engine
// + daemon running or the full Docker Desktop app — no dependency on
// the `docker` binary being on PATH at all.
func NewClient() (*Client, error) {
	cli, err := client.New(
		client.FromEnv,
		client.WithUserAgent("toolctl/1.0.0"),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to Docker: %w (is Docker running?)", err)
	}
	return &Client{cli: cli}, nil
}

// Close releases the underlying connection.
func (c *Client) Close() error {
	return c.cli.Close()
}

// EnsureImage checks whether imageRef is present locally and pulls it
// if not, streaming pull progress to stderr. This is what makes
// toolctl plug-and-play: first run self-provisions the toolbox image.
func (c *Client) EnsureImage(ctx context.Context, imageRef string) error {
	if _, err := c.cli.ImageInspect(ctx, imageRef); err == nil {
		return nil // already present
	}

	fmt.Fprintf(os.Stderr, "Image %s not found locally, pulling...\n", imageRef)
	pull, err := c.cli.ImagePull(ctx, imageRef, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pulling image %s: %w", imageRef, err)
	}
	defer pull.Close()

	// Drain the pull's JSON progress stream to stderr rather than
	// parsing it — good enough for a CLI tool's terminal output.
	scanner := bufio.NewScanner(pull)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		fmt.Fprintln(os.Stderr, scanner.Text())
	}
	return scanner.Err()
}

// RunOptions configures a single ephemeral container invocation.
type RunOptions struct {
	// Image is the toolbox image to run.
	Image string
	// Cmd is the full command + args to execute, e.g.
	// []string{"yt-dlp", "-x", "--audio-format", "mp3", url}.
	Cmd []string
	// HostMountPath is the host folder bound to ContainerDataDir.
	HostMountPath string
	// WorkDir sets the container's working directory. Defaults to
	// ContainerDataDir when empty.
	WorkDir string
}

// Run creates a container, streams its combined stdout/stderr to the
// current process's stdout/stderr, waits for it to exit, and removes
// it. It returns an error if the container exits non-zero.
//
// NOTE: ContainerCreateOptions and ContainerWaitOptions/Result are new
// (post-v29) option-struct types on a young, independently-versioned
// module. The field names below are believed correct as of this
// writing but haven't been compiled against a live copy of the module
// in this environment — if `go build` reports a field mismatch here,
// run `go doc github.com/moby/moby/client ContainerCreateOptions` (and
// ContainerWaitOptions/ContainerWaitResult) to confirm the exact shape
// and adjust.
func (c *Client) Run(ctx context.Context, opts RunOptions) error {
	workDir := opts.WorkDir
	if workDir == "" {
		workDir = ContainerDataDir
	}

	created, err := c.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:      opts.Image,
			Cmd:        opts.Cmd,
			WorkingDir: workDir,
			Tty:        false,
		},
		HostConfig: &container.HostConfig{
			Mounts: []mount.Mount{
				{
					Type:   mount.TypeBind,
					Source: opts.HostMountPath,
					Target: ContainerDataDir,
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("creating container: %w", err)
	}
	defer func() {
		_, _ = c.cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	}()

	if _, err := c.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("starting container: %w", err)
	}

	logs, err := c.cli.ContainerLogs(ctx, created.ID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil {
		return fmt.Errorf("attaching to container logs: %w", err)
	}
	defer logs.Close()
	// Docker multiplexes stdout/stderr with an 8-byte header per frame
	// when Tty is false; demux splits them back out.
	go demux(logs, os.Stdout, os.Stderr)

	wait := c.cli.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})
	select {
	case err := <-wait.ErrCh:
		if err != nil {
			return fmt.Errorf("waiting for container: %w", err)
		}
	case status := <-wait.StatusCh:
		if status.StatusCode != 0 {
			return fmt.Errorf("command exited with status %d", status.StatusCode)
		}
	}
	return nil
}

// demux copies Docker's multiplexed log stream to separate stdout/stderr
// writers. Kept local and minimal rather than pulling in a stdcopy
// package dependency for one call site.
func demux(src io.Reader, stdout, stderr io.Writer) {
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(src, header); err != nil {
			return
		}
		size := int(header[4])<<24 | int(header[5])<<16 | int(header[6])<<8 | int(header[7])
		dst := stdout
		if header[0] == 2 {
			dst = stderr
		}
		if _, err := io.CopyN(dst, src, int64(size)); err != nil {
			return
		}
	}
}
