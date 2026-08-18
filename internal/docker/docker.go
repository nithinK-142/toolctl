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
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// ContainerDataDir is the fixed path inside every tool container that
// the host mount path is bound to.
const (
	ContainerDataDir = "/data"
	ToolNetworkName  = "toolctl-net"
	POTProviderName  = "toolctl-bgutil"
	POTProviderPort  = 4416
)

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

// EnsurePOTProvider pulls and starts the managed bgutil PO-token provider.
// The provider is kept on a private Docker network and is not published to
// the host. The yt-dlp container resolves the provider endpoint dynamically
// from Docker network state and joins the same private network.
func (c *Client) EnsurePOTProvider(ctx context.Context, imageRef string) error {
	if imageRef == "" {
		return errors.New("PO-token provider image cannot be empty")
	}
	if err := c.EnsureImage(ctx, imageRef); err != nil {
		return err
	}

	networkID, err := c.ensureNetwork(ctx)
	if err != nil {
		return err
	}

	inspectResult, err := c.cli.ContainerInspect(ctx, POTProviderName, client.ContainerInspectOptions{})
	var inspect container.InspectResponse
	if err == nil {
		inspect = inspectResult.Container
	}
	if err == nil {
		if inspect.Config == nil || inspect.Config.Image != imageRef {
			if inspect.State != nil && inspect.State.Running {
				if _, err := c.cli.ContainerStop(ctx, inspect.ID, client.ContainerStopOptions{}); err != nil {
					return fmt.Errorf("stopping stale PO-token provider: %w", err)
				}
			}
			if _, err := c.cli.ContainerRemove(ctx, inspect.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
				return fmt.Errorf("removing stale PO-token provider: %w", err)
			}
			err = errdefs.ErrNotFound
		} else if inspect.State != nil && inspect.State.Running {
			if err := c.ensureContainerNetwork(ctx, inspect.ID, networkID); err != nil {
				return err
			}
			if inspect.State.Health == nil {
				return nil
			}
			return c.waitForPOTProvider(ctx, inspect.ID)
		} else {
			if _, err := c.cli.ContainerStart(ctx, inspect.ID, client.ContainerStartOptions{}); err != nil {
				return fmt.Errorf("starting PO-token provider: %w", err)
			}
			if err := c.ensureContainerNetwork(ctx, inspect.ID, networkID); err != nil {
				return err
			}
			return c.waitForPOTProvider(ctx, inspect.ID)
		}
	}
	if !errdefs.IsNotFound(err) {
		return fmt.Errorf("inspecting PO-token provider: %w", err)
	}

	created, err := c.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: POTProviderName,
		Config: &container.Config{
			Image: imageRef,
			Healthcheck: &container.HealthConfig{
				Test:     []string{"CMD", "deno", "eval", "fetch('http://127.0.0.1:4416/ping').then(r => Deno.exit(r.ok ? 0 : 1)).catch(() => Deno.exit(1))"},
				Interval: 2 * time.Second,
				Timeout:  2 * time.Second,
				Retries:  15,
			},
		},
		HostConfig: &container.HostConfig{
			RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
		},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				ToolNetworkName: &network.EndpointSettings{NetworkID: networkID},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("creating PO-token provider: %w", err)
	}
	if _, err := c.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("starting PO-token provider: %w", err)
	}
	return c.waitForPOTProvider(ctx, created.ID)
}

func (c *Client) POTProviderURL(ctx context.Context) (string, error) {
	inspectResult, err := c.cli.ContainerInspect(ctx, POTProviderName, client.ContainerInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("inspecting PO-token provider endpoint: %w", err)
	}
	inspect := inspectResult.Container
	if inspect.NetworkSettings == nil || inspect.NetworkSettings.Networks == nil {
		return "", errors.New("PO-token provider has no Docker network settings")
	}
	endpoint, ok := inspect.NetworkSettings.Networks[ToolNetworkName]
	if !ok || endpoint == nil {
		return "", fmt.Errorf("PO-token provider is not connected to Docker network %s", ToolNetworkName)
	}
	if !endpoint.IPAddress.IsValid() || !endpoint.IPAddress.Is4() {
		return "", fmt.Errorf("PO-token provider has no IPv4 address on Docker network %s", ToolNetworkName)
	}

	return "http://" + net.JoinHostPort(endpoint.IPAddress.String(), fmt.Sprintf("%d", POTProviderPort)), nil
}

func (c *Client) ensureNetwork(ctx context.Context) (string, error) {
	nets, err := c.cli.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing Docker networks: %w", err)
	}

	for _, n := range nets.Items {
		if n.Name == ToolNetworkName {
			return n.ID, nil
		}
	}
	created, err := c.cli.NetworkCreate(ctx, ToolNetworkName, client.NetworkCreateOptions{
		Driver: "bridge",
	})
	if err != nil {
		return "", fmt.Errorf("creating Docker network %s: %w", ToolNetworkName, err)
	}
	return created.ID, nil
}

func (c *Client) ensureContainerNetwork(ctx context.Context, containerID, networkID string) error {
	if networkID == "" {
		return errors.New("Docker network ID is empty")
	}
	inspectResult, err := c.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspecting PO-token provider networking: %w", err)
	}
	inspect := inspectResult.Container
	if inspect.NetworkSettings != nil && inspect.NetworkSettings.Networks != nil {
		for _, endpoint := range inspect.NetworkSettings.Networks {
			if endpoint != nil && endpoint.NetworkID == networkID {
				return nil
			}
		}
	}
	if _, err := c.cli.NetworkConnect(ctx, networkID, client.NetworkConnectOptions{
		Container:      containerID,
		EndpointConfig: &network.EndpointSettings{},
	}); err != nil {
		return fmt.Errorf("connecting PO-token provider to %s: %w", ToolNetworkName, err)
	}
	return nil
}

func (c *Client) waitForPOTProvider(ctx context.Context, containerID string) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	for {
		inspectResult, err := c.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
		if err != nil {
			return fmt.Errorf("inspecting PO-token provider readiness: %w", err)
		}
		inspect := inspectResult.Container
		if inspect.State != nil {
			if !inspect.State.Running {
				return fmt.Errorf("PO-token provider exited before becoming ready (exit code %d)", inspect.State.ExitCode)
			}
			if inspect.State.Health != nil && inspect.State.Health.Status == "healthy" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for PO-token provider: %w", ctx.Err())
		case <-deadline.C:
			return errors.New("timed out waiting for PO-token provider to become healthy")
		case <-ticker.C:
		}
	}
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
	// Network optionally attaches the container to a named Docker network.
	Network string
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

	createOpts := client.ContainerCreateOptions{
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
	}
	if opts.Network != "" {
		createOpts.NetworkingConfig = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				opts.Network: &network.EndpointSettings{},
			},
		}
	}
	created, err := c.cli.ContainerCreate(ctx, createOpts)
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
