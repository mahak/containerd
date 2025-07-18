/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	criconfig "github.com/containerd/containerd/v2/internal/cri/config"
	crilabels "github.com/containerd/containerd/v2/internal/cri/labels"
	"github.com/containerd/containerd/v2/internal/cri/server/podsandbox"
	containerdio "github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/netns"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/typeurl/v2"
	"golang.org/x/sync/errgroup"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	cio "github.com/containerd/containerd/v2/internal/cri/io"
	containerstore "github.com/containerd/containerd/v2/internal/cri/store/container"
	sandboxstore "github.com/containerd/containerd/v2/internal/cri/store/sandbox"
	ctrdutil "github.com/containerd/containerd/v2/internal/cri/util"
)

// NOTE: The recovery logic has following assumption: when the cri plugin is down:
// 1) Files (e.g. root directory, netns) and checkpoint maintained by the plugin MUST NOT be
// touched. Or else, recovery logic for those containers/sandboxes may return error.
// 2) Containerd containers may be deleted, but SHOULD NOT be added. Or else, recovery logic
// for the newly added container/sandbox will return error, because there is no corresponding root
// directory created.
// 3) Containerd container tasks may exit or be stopped, deleted. Even though current logic could
// tolerant tasks being created or started, we prefer that not to happen.

// recover recovers system state from containerd and status checkpoint.
func (c *criService) recover(ctx context.Context) error {
	// Recover all sandboxes.
	sandboxes, err := c.client.Containers(ctx, filterLabel(crilabels.ContainerKindLabel, crilabels.ContainerKindSandbox))
	if err != nil {
		return fmt.Errorf("failed to list sandbox containers: %w", err)
	}

	podSandboxController, err := c.sandboxService.SandboxController(string(criconfig.ModePodSandbox))
	if err != nil {
		return fmt.Errorf("failed to get podsanbox controller %v", err)
	}
	podSandboxLoader, ok := podSandboxController.(podSandboxRecover)
	if !ok {
		log.G(ctx).Fatal("pod sandbox controller doesn't support recovery")
	}

	eg, ctx2 := errgroup.WithContext(ctx)
	for _, sandbox := range sandboxes {
		eg.Go(func() error {
			sb, err := podSandboxLoader.RecoverContainer(ctx2, sandbox)
			if err != nil {
				log.G(ctx2).
					WithError(err).
					WithField("sandbox", sandbox.ID()).
					Error("Failed to load sandbox")

				return nil
			}
			log.G(ctx2).Debugf("Loaded sandbox %+v", sb)
			if err := c.sandboxStore.Add(sb); err != nil {
				return fmt.Errorf("failed to add sandbox %q to store: %w", sandbox.ID(), err)
			}
			if err := c.sandboxNameIndex.Reserve(sb.Name, sb.ID); err != nil {
				return fmt.Errorf("failed to reserve sandbox name %q: %w", sb.Name, err)
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}

	// Recover sandboxes in the new SandboxStore
	storedSandboxes, err := c.client.SandboxStore().List(ctx)
	if err != nil {
		return fmt.Errorf("failed to list sandboxes from API: %w", err)
	}
	for _, sbx := range storedSandboxes {
		if _, err := c.sandboxStore.Get(sbx.ID); err == nil {
			continue
		}

		metadata := sandboxstore.Metadata{}
		err := sbx.GetExtension(podsandbox.MetadataKey, &metadata)
		if err != nil {
			if errors.Is(err, errdefs.ErrNotFound) {
				log.G(ctx).WithError(err).Errorf("failed to get metadata for stored sandbox %q", sbx.ID)
				// Since commit https://github.com/containerd/containerd/pull/11612 has been merged metadata may not be nil.
				// Before 1162 we should delete leaked sandbox from sandbox store to make sure containerd can start successfully.
				err = c.client.SandboxStore().Delete(ctx, sbx.ID)
				if err != nil {
					log.G(ctx).WithError(err).Errorf("failed to delete sandbox %q, in response to failure to retrieve metadata for sandbox", sbx.ID)
				}
				continue
			}
			return fmt.Errorf("failed to get metadata for stored sandbox %q: %w", sbx.ID, err)
		}

		var (
			state    = sandboxstore.StateUnknown
			endpoint sandboxstore.Endpoint
		)

		status, err := c.sandboxService.SandboxStatus(ctx, sbx.Sandboxer, sbx.ID, false)
		if err != nil {
			log.G(ctx).
				WithError(err).
				WithField("sandbox", sbx.ID).
				Error("failed to recover sandbox state")

			if errdefs.IsNotFound(err) {
				state = sandboxstore.StateNotReady
			}
		} else {
			endpoint.Version = status.Version
			endpoint.Address = status.Address
			if code, ok := runtime.PodSandboxState_value[status.State]; ok {
				if code == int32(runtime.PodSandboxState_SANDBOX_READY) {
					state = sandboxstore.StateReady
				} else if code == int32(runtime.PodSandboxState_SANDBOX_NOTREADY) {
					state = sandboxstore.StateNotReady
				}
			}
		}

		sb := sandboxstore.NewSandbox(metadata, sandboxstore.Status{State: state})
		sb.Sandboxer = sbx.Sandboxer
		sb.Endpoint = endpoint

		// Load network namespace.
		sb.NetNS = getNetNS(&metadata)

		if err := c.sandboxStore.Add(sb); err != nil {
			return fmt.Errorf("failed to add stored sandbox %q to store: %w", sbx.ID, err)
		}
	}

	for _, sb := range c.sandboxStore.List() {
		status := sb.Status.Get()
		if status.State == sandboxstore.StateNotReady {
			continue
		}
		exitCh, err := c.sandboxService.WaitSandbox(ctrdutil.NamespacedContext(), sb.Sandboxer, sb.ID)
		if err != nil {
			log.G(ctx).WithError(err).Error("failed to wait sandbox")
			continue
		}
		c.startSandboxExitMonitor(context.Background(), sb.ID, exitCh)
	}
	// Recover all containers.
	containers, err := c.client.Containers(ctx, filterLabel(crilabels.ContainerKindLabel, crilabels.ContainerKindContainer))
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}
	eg, ctx2 = errgroup.WithContext(ctx)
	for _, container := range containers {
		eg.Go(func() error {
			cntr, exitCh, pid, err := c.loadContainer(ctx2, container)
			if err != nil {
				log.G(ctx2).
					WithError(err).
					WithField("container", container.ID()).
					Error("Failed to load container")

				return nil
			}
			log.G(ctx2).Debugf("Loaded container %+v", cntr)
			if err := c.containerStore.Add(cntr); err != nil {
				return fmt.Errorf("failed to add container %q to store: %w", container.ID(), err)
			}
			if exitCh != nil {
				// Start the exit monitor. This should run after that container has been added to the container store.
				c.startContainerExitMonitor(context.Background(), cntr.ID, pid, exitCh)
			}
			if err := c.containerNameIndex.Reserve(cntr.Name, cntr.ID); err != nil {
				return fmt.Errorf("failed to reserve container name %q: %w", cntr.Name, err)
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}
	// Recover all images.
	if err := c.ImageService.CheckImages(ctx); err != nil {
		return fmt.Errorf("failed to check images: %w", err)
	}

	// It's possible that containerd containers are deleted unexpectedly. In that case,
	// we can't even get metadata, we should cleanup orphaned sandbox/container directories
	// with best effort.

	// Cleanup orphaned sandbox and container directories without corresponding containerd container.
	for _, cleanup := range []struct {
		cntrs  []containerd.Container
		base   string
		errMsg string
	}{
		{
			cntrs:  sandboxes,
			base:   filepath.Join(c.config.RootDir, sandboxesDir),
			errMsg: "failed to cleanup orphaned sandbox directories",
		},
		{
			cntrs:  sandboxes,
			base:   filepath.Join(c.config.StateDir, sandboxesDir),
			errMsg: "failed to cleanup orphaned volatile sandbox directories",
		},
		{
			cntrs:  containers,
			base:   filepath.Join(c.config.RootDir, containersDir),
			errMsg: "failed to cleanup orphaned container directories",
		},
		{
			cntrs:  containers,
			base:   filepath.Join(c.config.StateDir, containersDir),
			errMsg: "failed to cleanup orphaned volatile container directories",
		},
	} {
		if err := cleanupOrphanedIDDirs(ctx, cleanup.cntrs, cleanup.base); err != nil {
			return fmt.Errorf("%s: %w", cleanup.errMsg, err)
		}
	}
	return nil
}

// loadContainerTimeout is the default timeout for loading a container/sandbox.
// One container/sandbox hangs (e.g. containerd#2438) should not affect other
// containers/sandboxes.
// Most CRI container/sandbox related operations are per container, the ones
// which handle multiple containers at a time are:
// * ListPodSandboxes: Don't talk with containerd services.
// * ListContainers: Don't talk with containerd services.
// * ListContainerStats: Not in critical code path, a default timeout will
// be applied at CRI level.
// * Recovery logic: We should set a time for each container/sandbox recovery.
// * Event monitor: We should set a timeout for each container/sandbox event handling.
const loadContainerTimeout = 10 * time.Second

// loadContainer loads container from containerd and status checkpoint.
func (c *criService) loadContainer(ctx context.Context, cntr containerd.Container) (containerstore.Container, <-chan containerd.ExitStatus, uint32, error) {
	var exitCh <-chan containerd.ExitStatus
	var statusPid uint32
	ctx, cancel := context.WithTimeout(ctx, loadContainerTimeout)
	defer cancel()
	id := cntr.ID()
	containerDir := c.getContainerRootDir(id)
	var container containerstore.Container
	// Load container metadata.
	exts, err := cntr.Extensions(ctx)
	if err != nil {
		return container, nil, 0, fmt.Errorf("failed to get container extensions: %w", err)
	}
	ext, ok := exts[crilabels.ContainerMetadataExtension]
	if !ok {
		return container, nil, 0, fmt.Errorf("metadata extension %q not found", crilabels.ContainerMetadataExtension)
	}
	data, err := typeurl.UnmarshalAny(ext)
	if err != nil {
		return container, nil, 0, fmt.Errorf("failed to unmarshal metadata extension %q: %w", ext, err)
	}
	meta := data.(*containerstore.Metadata)

	// Load status from checkpoint.
	status, err := containerstore.LoadStatus(containerDir, id)
	if err != nil {
		log.G(ctx).WithError(err).Warnf("Failed to load container status for %q", id)
		status = unknownContainerStatus()
	}

	var containerIO *cio.ContainerIO
	err = func() error {
		// Load up-to-date status from containerd.
		t, err := cntr.Task(ctx, func(fifos *containerdio.FIFOSet) (_ containerdio.IO, err error) {
			stdoutWC, stderrWC, err := c.createContainerLoggers(meta.LogPath, meta.Config.GetTty())
			if err != nil {
				return nil, err
			}
			defer func() {
				if err != nil {
					if stdoutWC != nil {
						stdoutWC.Close()
					}
					if stderrWC != nil {
						stderrWC.Close()
					}
				}
			}()
			containerIO, err = cio.NewContainerIO(id,
				cio.WithFIFOs(fifos),
			)
			if err != nil {
				return nil, err
			}
			containerIO.AddOutput("log", stdoutWC, stderrWC)
			containerIO.Pipe()
			return containerIO, nil
		})
		if err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("failed to load task: %w", err)
		}
		var s containerd.Status
		var notFound bool
		if errdefs.IsNotFound(err) {
			// Task is not found.
			notFound = true
		} else {
			// Task is found. Get task status.
			s, err = t.Status(ctx)
			if err != nil {
				// It's still possible that task is deleted during this window.
				if !errdefs.IsNotFound(err) {
					return fmt.Errorf("failed to get task status: %w", err)
				}
				notFound = true
			}
		}
		if notFound {
			// Task is not created or has been deleted, use the checkpointed status
			// to generate container status.
			switch status.State() {
			case runtime.ContainerState_CONTAINER_CREATED:
				// NOTE: Another possibility is that we've tried to start the container, but
				// containerd got restarted during that. In that case, we still
				// treat the container as `CREATED`.
				containerIO, err = c.createContainerIO(id, meta.SandboxID, meta.Config)
				if err != nil {
					return fmt.Errorf("failed to create container io: %w", err)
				}
			case runtime.ContainerState_CONTAINER_RUNNING:
				// Container was in running state, but its task has been deleted,
				// set unknown exited state. Container io is not needed in this case.
				status.FinishedAt = time.Now().UnixNano()
				status.ExitCode = unknownExitCode
				status.Reason = unknownExitReason
			default:
				// Container is in exited/unknown state, return the status as it is.
			}
		} else {
			// Task status is found. Update container status based on the up-to-date task status.
			switch s.Status {
			case containerd.Created:
				// Task has been created, but not started yet. This could only happen if containerd
				// gets restarted during container start.
				// Container must be in `CREATED` state.
				if _, err := t.Delete(ctx, containerd.WithProcessKill); err != nil && !errdefs.IsNotFound(err) {
					return fmt.Errorf("failed to delete task: %w", err)
				}
				if status.State() != runtime.ContainerState_CONTAINER_CREATED {
					return fmt.Errorf("unexpected container state for created task: %q", status.State())
				}
			case containerd.Running:
				// Task is running. Container must be in `RUNNING` state, based on our assumption that
				// "task should not be started when containerd is down".
				switch status.State() {
				case runtime.ContainerState_CONTAINER_EXITED:
					return fmt.Errorf("unexpected container state for running task: %q", status.State())
				case runtime.ContainerState_CONTAINER_RUNNING:
				default:
					// This may happen if containerd gets restarted after task is started, but
					// before status is checkpointed.
					status.StartedAt = time.Now().UnixNano()
					status.Pid = t.Pid()
					statusPid = t.Pid()
				}
				// Wait for the task for exit monitor.
				// wait is a long running background request, no timeout needed.
				exitCh, err = t.Wait(ctrdutil.NamespacedContext())
				if err != nil {
					exitCh = nil
					if !errdefs.IsNotFound(err) {
						return fmt.Errorf("failed to wait for task: %w", err)
					}
					// Container was in running state, but its task has been deleted,
					// set unknown exited state.
					status.FinishedAt = time.Now().UnixNano()
					status.ExitCode = unknownExitCode
					status.Reason = unknownExitReason
				}
			case containerd.Stopped:
				// Task is stopped. Update status and delete the task.
				if _, err := t.Delete(ctx, containerd.WithProcessKill); err != nil && !errdefs.IsNotFound(err) {
					return fmt.Errorf("failed to delete task: %w", err)
				}
				status.FinishedAt = s.ExitTime.UnixNano()
				status.ExitCode = int32(s.ExitStatus)
			default:
				return fmt.Errorf("unexpected task status %q", s.Status)
			}
		}
		return nil
	}()
	if err != nil {
		log.G(ctx).WithError(err).Errorf("Failed to load container status for %q", id)
		// Only set the unknown field in this case, because other fields may
		// contain useful information loaded from the checkpoint.
		status.Unknown = true
	}
	opts := []containerstore.Opts{
		containerstore.WithStatus(status, containerDir),
		containerstore.WithContainer(cntr),
	}
	// containerIO could be nil for container in unknown state.
	if containerIO != nil {
		opts = append(opts, containerstore.WithContainerIO(containerIO))
	}
	container, err = containerstore.NewContainer(*meta, opts...)
	return container, exitCh, statusPid, err
}

// podSandboxRecover is an additional interface implemented by podsandbox/ controller to handle
// Pod sandbox containers recovery.
type podSandboxRecover interface {
	RecoverContainer(ctx context.Context, cntr containerd.Container) (sandboxstore.Sandbox, error)
}

func getNetNS(meta *sandboxstore.Metadata) *netns.NetNS {
	// Don't need to load netns for host network sandbox.
	if hostNetwork(meta.Config) {
		return nil
	}
	return netns.LoadNetNS(meta.NetNSPath)
}

func cleanupOrphanedIDDirs(ctx context.Context, cntrs []containerd.Container, base string) error {
	// Cleanup orphaned id directories.
	dirs, err := os.ReadDir(base)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to read base directory: %w", err)
	}
	idsMap := make(map[string]containerd.Container)
	for _, cntr := range cntrs {
		idsMap[cntr.ID()] = cntr
	}
	for _, d := range dirs {
		if !d.IsDir() {
			log.G(ctx).Warnf("Invalid file %q found in base directory %q", d.Name(), base)
			continue
		}
		if _, ok := idsMap[d.Name()]; ok {
			// Do not remove id directory if corresponding container is found.
			continue
		}
		dir := filepath.Join(base, d.Name())
		if err := ensureRemoveAll(ctx, dir); err != nil {
			log.G(ctx).WithError(err).Warnf("Failed to remove id directory %q", dir)
		} else {
			log.G(ctx).Debugf("Cleanup orphaned id directory %q", dir)
		}
	}
	return nil
}

func (c *criService) createContainerIO(containerID, sandboxID string, config *runtime.ContainerConfig) (*cio.ContainerIO, error) {
	if config == nil {
		return nil, fmt.Errorf("ContainerConfig should not be nil when create container io")
	}
	sb, err := c.sandboxStore.Get(sandboxID)
	if err != nil {
		return nil, fmt.Errorf("an error occurred when try to find sandbox %q: %w", sandboxID, err)
	}
	ociRuntime, err := c.config.GetSandboxRuntime(sb.Config, sb.Metadata.RuntimeHandler)
	if err != nil {
		return nil, fmt.Errorf("failed to get sandbox runtime: %w", err)
	}
	var containerIO *cio.ContainerIO
	switch ociRuntime.IOType {
	case criconfig.IOTypeStreaming:
		containerIO, err = cio.NewContainerIO(containerID,
			cio.WithStreams(sb.Endpoint.Address, config.GetTty(), config.GetStdin()))
	default:
		volatileContainerRootDir := c.getVolatileContainerRootDir(containerID)
		containerIO, err = cio.NewContainerIO(containerID,
			cio.WithNewFIFOs(volatileContainerRootDir, config.GetTty(), config.GetStdin()))
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create container io: %w", err)
	}
	return containerIO, nil
}
