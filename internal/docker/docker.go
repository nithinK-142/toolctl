// Package docker wraps the subset of the Docker Engine API client that
// toolctl needs: resolving the Engine client, ensuring
// the toolbox image is present, and running one-shot ephemeral containers
// with a single bind mount.
//
// Built against github.com/moby/moby/client — the maintained successor to
// the deprecated github.com/docker/docker/client. See go.mod for why.
package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
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

// NewClient resolves a Docker Engine client from supported environment
// settings. API version negotiation happens automatically on the first
// request. The Docker CLI is not required at runtime.
func NewClient() (*Client, error) {
	// Docker Desktop for Linux uses a per-user socket instead of the
	// Engine default /var/run/docker.sock. The Docker CLI manages its
	// context separately, but the Go SDK intentionally reads environment
	// configuration only. Detect the standard Desktop socket when the
	// caller has not explicitly set DOCKER_HOST.
	if os.Getenv("DOCKER_HOST") == "" && runtime.GOOS == "linux" {
		if home, err := os.UserHomeDir(); err == nil {
			desktopSocket := filepath.Join(home, ".docker", "desktop", "docker.sock")
			if _, err := os.Stat(desktopSocket); err == nil {
				_ = os.Setenv("DOCKER_HOST", "unix://"+desktopSocket)
			}
		}
	}

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
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("inspecting image %s: %w", imageRef, err)
	}

	fmt.Fprintf(os.Stderr, "Image %s not found locally, pulling...\n", imageRef)
	pull, err := c.cli.ImagePull(ctx, imageRef, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pulling image %s: %w", imageRef, err)
	}
	defer pull.Close()

	// Drain the pull's JSON progress stream to stderr rather than
	// parsing it — good enough for a CLI tool's terminal output.
	type pullMessage struct {
		Status      string `json:"status"`
		Progress    string `json:"progress"`
		Error       string `json:"error"`
		ErrorDetail struct {
			Message string `json:"message"`
		} `json:"errorDetail"`
	}

	decoder := json.NewDecoder(pull)
	for {
		var msg pullMessage
		if err := decoder.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("reading image pull response: %w", err)
		}
		if msg.Error != "" {
			if msg.ErrorDetail.Message != "" {
				return fmt.Errorf("pulling image %s: %s", imageRef, msg.ErrorDetail.Message)
			}
			return fmt.Errorf("pulling image %s: %s", imageRef, msg.Error)
		}
		if msg.Status != "" {
			if msg.Progress != "" {
				fmt.Fprintf(os.Stderr, "%s %s\n", msg.Status, msg.Progress)
			} else {
				fmt.Fprintln(os.Stderr, msg.Status)
			}
		}
	}
	return nil
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
func (c *Client) Run(ctx context.Context, opts RunOptions) error {
	if len(opts.Cmd) == 0 {
		return errors.New("container command cannot be empty")
	}
	if opts.Image == "" {
		return errors.New("container image cannot be empty")
	}
	if opts.HostMountPath == "" {
		return errors.New("host mount path cannot be empty")
	}

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
			Mounts: []mount.Mount{{
				Type:     mount.TypeBind,
				Source:   opts.HostMountPath,
				Target:   ContainerDataDir,
				ReadOnly: false,
			}},
		},
	})
	if err != nil {
		return fmt.Errorf("creating container: %w", err)
	}

	remove := func() {
		cleanupCtx := context.Background()
		if _, err := c.cli.ContainerRemove(cleanupCtx, created.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
			fmt.Fprintf(os.Stderr, "warning: removing container %s: %v\n", created.ID[:12], err)
		}
	}
	defer remove()

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

	if _, err := stdcopy.StdCopy(os.Stdout, os.Stderr, logs); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("reading container logs: %w", err)
	}

	wait := c.cli.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})
	select {
	case err := <-wait.Error:
		if err != nil {
			return fmt.Errorf("waiting for container: %w", err)
		}
	case result := <-wait.Result:
		if result.StatusCode != 0 {
			return fmt.Errorf("command exited with status %d", result.StatusCode)
		}
	case <-ctx.Done():
		return fmt.Errorf("container command canceled: %w", ctx.Err())
	}
	return nil
}
