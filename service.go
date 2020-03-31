package wasm

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

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/cgroups"
	eventstypes "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/pkg/stdio"
	"github.com/containerd/containerd/runtime/v2/shim"
	taskAPI "github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/typeurl"
	"github.com/dmcgowan/containerd-wasm/wasmtime"
	ptypes "github.com/gogo/protobuf/types"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

var (
	_     = (taskAPI.TaskService)(&service{})
	empty = &ptypes.Empty{}
)

// group labels specifies how the shim groups services.
// currently supports a runc.v2 specific .group label and the
// standard k8s pod label.  Order matters in this list
var groupLabels = []string{
	"io.containerd.runc.v2.group",
	"io.kubernetes.cri.sandbox-id",
}

type spec struct {
	Annotations map[string]string `json:"annotations,omitempty"`
}

// New returns a new shim service that can be used via GRPC
func New(ctx context.Context, id string, publisher shim.Publisher, cancel func()) (shim.Shim, error) {
	ep, err := wasmtime.NewOOMEpoller(publisher)
	if err != nil {
		return nil, err
	}
	go ep.Run(ctx)
	s := &service{
		id:         id,
		context:    ctx,
		events:     make(chan interface{}, 128),
		ec:         make(chan wasmtime.Exit),
		ep:         ep,
		cancel:     cancel,
		containers: make(map[string]*wasmtime.Container),
		log:        log.GetLogger(context.TODO()),
	}
	go s.processExits()
	if err := s.initPlatform(); err != nil {
		cancel()
		return nil, errors.Wrap(err, "failed to initialized platform behavior")
	}
	go s.forward(ctx, publisher)
	return s, nil
}

// service is the shim implementation of a remote shim over GRPC
type service struct {
	mu          sync.Mutex
	eventSendMu sync.Mutex

	context  context.Context
	events   chan interface{}
	platform stdio.Platform
	ec       chan wasmtime.Exit
	ep       *wasmtime.Epoller
	log      *logrus.Entry

	// id only used in cleanup case
	id string

	containers map[string]*wasmtime.Container

	cancel func()
}

func newCommand(ctx context.Context, id, containerdBinary, containerdAddress string) (*exec.Cmd, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	args := []string{
		"-namespace", ns,
		"-id", id,
		"-address", containerdAddress,
	}
	cmd := exec.Command(self, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GOMAXPROCS=4")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	return cmd, nil
}

func readSpec() (*spec, error) {
	f, err := os.Open("config.json")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var s spec
	if err := json.NewDecoder(f).Decode(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *service) StartShim(ctx context.Context, id, containerdBinary, containerdAddress, containerdTTRPCAddress string) (_ string, retErr error) {
	cmd, err := newCommand(ctx, id, containerdBinary, containerdAddress)
	if err != nil {
		return "", err
	}
	grouping := id
	spec, err := readSpec()
	if err != nil {
		return "", err
	}
	for _, group := range groupLabels {
		if groupID, ok := spec.Annotations[group]; ok {
			grouping = groupID
			break
		}
	}
	address, err := shim.SocketAddress(ctx, grouping)
	if err != nil {
		return "", err
	}
	socket, err := shim.NewSocket(address)
	if err != nil {
		if strings.Contains(err.Error(), "address already in use") {
			if err := shim.WriteAddress("address", address); err != nil {
				return "", err
			}
			return address, nil
		}
		return "", err
	}
	defer socket.Close()
	f, err := socket.File()
	if err != nil {
		return "", err
	}
	defer f.Close()

	cmd.ExtraFiles = append(cmd.ExtraFiles, f)

	if err := cmd.Start(); err != nil {
		return "", err
	}
	defer func() {
		if retErr != nil {
			cmd.Process.Kill()
		}
	}()
	// make sure to wait after start
	go cmd.Wait()
	if err := shim.WriteAddress("address", address); err != nil {
		return "", err
	}
	if err := shim.SetScore(cmd.Process.Pid); err != nil {
		return "", errors.Wrap(err, "failed to set OOM Score on shim")
	}
	return address, nil
}

func (s *service) Cleanup(ctx context.Context) (*taskAPI.DeleteResponse, error) {
	s.log.Info("wasm Cleanup")

	return &taskAPI.DeleteResponse{
		ExitedAt:   time.Now(),
		ExitStatus: 128,
	}, nil
}

// Create a new initial process and container with wasmtime
func (s *service) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (_ *taskAPI.CreateTaskResponse, err error) {
	s.log.Info("wasm Create")
	s.mu.Lock()
	defer s.mu.Unlock()

	container, err := wasmtime.NewContainer(ctx, s.platform, r, s.ec)
	if err != nil {
		return nil, err
	}

	s.containers[r.ID] = container

	s.send(&eventstypes.TaskCreate{
		ContainerID: r.ID,
		Bundle:      r.Bundle,
		Rootfs:      r.Rootfs,
		IO: &eventstypes.TaskIO{
			Stdin:    r.Stdin,
			Stdout:   r.Stdout,
			Stderr:   r.Stderr,
			Terminal: r.Terminal,
		},
		Checkpoint: r.Checkpoint,
		Pid:        uint32(container.Pid()),
	})

	return &taskAPI.CreateTaskResponse{
		Pid: uint32(container.Pid()),
	}, nil
}

// Start a process
func (s *service) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	s.log.Info("wasm Start")
	container, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	// hold the send lock so that the start events are sent before any exit events in the error case
	s.eventSendMu.Lock()
	p, err := container.Start(ctx, r)
	if err != nil {
		s.eventSendMu.Unlock()
		return nil, errdefs.ToGRPC(err)
	}

	//if err := s.ep.Add(container.ID, container.Cgroup()); err != nil {
	//	logrus.WithError(err).Error("add cg to OOM monitor")
	//}
	switch r.ExecID {
	case "":
		s.send(&eventstypes.TaskStart{
			ContainerID: container.ID,
			Pid:         uint32(p.Pid()),
		})
	default:
		s.send(&eventstypes.TaskExecStarted{
			ContainerID: container.ID,
			ExecID:      r.ExecID,
			Pid:         uint32(p.Pid()),
		})
	}
	s.eventSendMu.Unlock()
	return &taskAPI.StartResponse{
		Pid: uint32(p.Pid()),
	}, nil
}

// Delete the initial process and container
func (s *service) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	s.log.Info("wasm Delete")
	container, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	p, err := container.Delete(ctx, r)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	// if we deleted our init task, close the platform and send the task delete event
	if r.ExecID == "" {
		s.mu.Lock()
		logrus.WithField("id", r.ID).Info("deleting container")
		delete(s.containers, r.ID)
		hasContainers := len(s.containers) > 0
		s.mu.Unlock()
		if s.platform != nil && !hasContainers {
			s.platform.Close()
		}
		s.send(&eventstypes.TaskDelete{
			ContainerID: container.ID,
			Pid:         uint32(p.Pid()),
			ExitStatus:  uint32(p.ExitStatus()),
			ExitedAt:    p.ExitedAt(),
		})
	}
	return &taskAPI.DeleteResponse{
		ExitStatus: uint32(p.ExitStatus()),
		ExitedAt:   p.ExitedAt(),
		Pid:        uint32(p.Pid()),
	}, nil
}

// Exec an additional process inside the container
func (s *service) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (*ptypes.Empty, error) {
	s.log.Info("wasm Exec")
	return nil, errdefs.ErrNotImplemented
	/*container, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	if container.ProcessExists(r.ExecID) {
		return nil, errdefs.ToGRPCf(errdefs.ErrAlreadyExists, "id %s", r.ExecID)
	}
	process, err := container.Exec(ctx, r)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	s.send(&eventstypes.TaskExecAdded{
		ContainerID: container.ID,
		ExecID:      process.ID(),
	})
	return empty, nil*/
}

// ResizePty of a process
func (s *service) ResizePty(ctx context.Context, r *taskAPI.ResizePtyRequest) (*ptypes.Empty, error) {
	s.log.Info("wasm ResizePty")
	return nil, errdefs.ErrNotImplemented
	/*container, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	if err := container.ResizePty(ctx, r); err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	return empty, nil*/
}

// State returns runtime state information for a process
func (s *service) State(ctx context.Context, r *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	container, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	p, err := container.Process(r.ExecID)
	if err != nil {
		return nil, err
	}
	st, err := p.Status(ctx)
	if err != nil {
		return nil, err
	}
	status := task.StatusUnknown
	switch st {
	case "created":
		status = task.StatusCreated
	case "running":
		status = task.StatusRunning
	case "stopped":
		status = task.StatusStopped
	case "paused":
		status = task.StatusPaused
	case "pausing":
		status = task.StatusPausing
	}
	sio := p.Stdio()
	s.log.Infof("wasm State %s", status)
	return &taskAPI.StateResponse{
		ID:         p.ID(),
		Bundle:     container.Bundle,
		Pid:        uint32(p.Pid()),
		Status:     status,
		Stdin:      sio.Stdin,
		Stdout:     sio.Stdout,
		Stderr:     sio.Stderr,
		Terminal:   sio.Terminal,
		ExitStatus: uint32(p.ExitStatus()),
		ExitedAt:   p.ExitedAt(),
	}, nil
}

// Pause the container
func (s *service) Pause(ctx context.Context, r *taskAPI.PauseRequest) (*ptypes.Empty, error) {

	s.log.Info("wasm Pause")
	return nil, errdefs.ErrNotImplemented
	/*container, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	if err := container.Pause(ctx); err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	s.send(&eventstypes.TaskPaused{
		ContainerID: container.ID,
	})
	return empty, nil*/
}

// Resume the container
func (s *service) Resume(ctx context.Context, r *taskAPI.ResumeRequest) (*ptypes.Empty, error) {
	s.log.Info("wasm Resume")
	return nil, errdefs.ErrNotImplemented
	/*container, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	if err := container.Resume(ctx); err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	s.send(&eventstypes.TaskResumed{
		ContainerID: container.ID,
	})
	return empty, nil*/
}

// Kill a process with the provided signal
func (s *service) Kill(ctx context.Context, r *taskAPI.KillRequest) (*ptypes.Empty, error) {
	s.log.Infof("wasm Kill %s", r.ID)
	container, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	if err := container.Kill(ctx, r); err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	return empty, nil
}

// Pids returns all pids inside the container
func (s *service) Pids(ctx context.Context, r *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	s.log.Info("wasm Pids")
	return nil, errdefs.ErrNotImplemented
	/*container, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	pids, err := s.getContainerPids(ctx, r.ID)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	var processes []*task.ProcessInfo
	for _, pid := range pids {
		pInfo := task.ProcessInfo{
			Pid: pid,
		}
		for _, p := range container.ExecdProcesses() {
			if p.Pid() == int(pid) {
				d := &options.ProcessDetails{
					ExecID: p.ID(),
				}
				a, err := typeurl.MarshalAny(d)
				if err != nil {
					return nil, errors.Wrapf(err, "failed to marshal process %d info", pid)
				}
				pInfo.Info = a
				break
			}
		}
		processes = append(processes, &pInfo)
	}
	return &taskAPI.PidsResponse{
		Processes: processes,
	}, nil*/
}

// CloseIO of a process
func (s *service) CloseIO(ctx context.Context, r *taskAPI.CloseIORequest) (*ptypes.Empty, error) {
	s.log.Info("wasm CloseIO")
	return nil, errdefs.ErrNotImplemented
	/*container, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	if err := container.CloseIO(ctx, r); err != nil {
		return nil, err
	}
	return empty, nil*/
}

// Checkpoint the container
func (s *service) Checkpoint(ctx context.Context, r *taskAPI.CheckpointTaskRequest) (*ptypes.Empty, error) {
	s.log.Info("wasm Checkpoint")
	return nil, errdefs.ErrNotImplemented
	/*container, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	if err := container.Checkpoint(ctx, r); err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	return empty, nil*/
}

// Update a running container
func (s *service) Update(ctx context.Context, r *taskAPI.UpdateTaskRequest) (*ptypes.Empty, error) {
	s.log.Info("wasm Update")
	return nil, errdefs.ErrNotImplemented
	/*container, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	if err := container.Update(ctx, r); err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	return empty, nil*/
}

// Wait for a process to exit
func (s *service) Wait(ctx context.Context, r *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	s.log.Info("wasm Wait")
	container, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	p, err := container.Process(r.ExecID)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	p.Wait()

	return &taskAPI.WaitResponse{
		ExitStatus: uint32(p.ExitStatus()),
		ExitedAt:   p.ExitedAt(),
	}, nil
}

// Connect returns shim information such as the shim's pid
func (s *service) Connect(ctx context.Context, r *taskAPI.ConnectRequest) (*taskAPI.ConnectResponse, error) {
	s.log.Info("wasm Connect")
	return nil, errdefs.ErrNotImplemented
	var pid int
	if container, err := s.getContainer(r.ID); err == nil {
		pid = container.Pid()
	}
	return &taskAPI.ConnectResponse{
		ShimPid: uint32(os.Getpid()),
		TaskPid: uint32(pid),
	}, nil
}

func (s *service) Shutdown(ctx context.Context, r *taskAPI.ShutdownRequest) (*ptypes.Empty, error) {
	s.log.Info("wasm Shutdown")
	s.mu.Lock()
	// return out if the shim is still servicing containers
	if len(s.containers) > 0 {
		s.mu.Unlock()
		return empty, nil
	}
	s.cancel()
	close(s.events)
	os.Exit(0)
	return empty, nil
}

func (s *service) Stats(ctx context.Context, r *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	s.log.Info("wasm Stats")
	container, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	cg := container.Cgroup()
	if cg == nil {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "cgroup does not exist")
	}
	stats, err := cg.Stat(cgroups.IgnoreNotExist)
	if err != nil {
		return nil, err
	}
	data, err := typeurl.MarshalAny(stats)
	if err != nil {
		return nil, err
	}
	return &taskAPI.StatsResponse{
		Stats: data,
	}, nil
}

func (s *service) send(evt interface{}) {
	s.events <- evt
}

func (s *service) sendL(evt interface{}) {
	s.eventSendMu.Lock()
	s.events <- evt
	s.eventSendMu.Unlock()
}

// TODO: provide way to check processes
func (s *service) processExits() {
	for e := range s.ec {
		s.checkProcesses(e)
	}
}

func (s *service) checkProcesses(e wasmtime.Exit) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, container := range s.containers {
		if container.HasPid(e.Pid) {
			//shouldKillAll, err := shouldKillAllOnExit(container.Bundle)
			//if err != nil {
			//	log.G(s.context).WithError(err).Error("failed to check shouldKillAll")
			//}

			for _, p := range container.All() {
				if p.Pid() == e.Pid {
					//if shouldKillAll {
					//	if ip, ok := p.(*proc.Init); ok {
					//		// Ensure all children are killed
					//		if err := ip.KillAll(s.context); err != nil {
					//			logrus.WithError(err).WithField("id", ip.ID()).
					//				Error("failed to kill init's children")
					//		}
					//	}
					//}
					p.SetExited(e.Status)
					s.sendL(&eventstypes.TaskExit{
						ContainerID: container.ID,
						ID:          p.ID(),
						Pid:         uint32(e.Pid),
						ExitStatus:  uint32(e.Status),
						ExitedAt:    p.ExitedAt(),
					})
					return
				}
			}
			return
		}
	}
}

func shouldKillAllOnExit(bundlePath string) (bool, error) {
	//var bundleSpec specs.Spec
	//bundleConfigContents, err := ioutil.ReadFile(filepath.Join(bundlePath, "config.json"))
	//if err != nil {
	//	return false, err
	//}
	//json.Unmarshal(bundleConfigContents, &bundleSpec)

	//if bundleSpec.Linux != nil {
	//	for _, ns := range bundleSpec.Linux.Namespaces {
	//		if ns.Type == specs.PIDNamespace && ns.Path == "" {
	//			return false, nil
	//		}
	//	}
	//}

	return true, nil
}

func (s *service) getContainerPids(ctx context.Context, id string) ([]uint32, error) {
	// TODO: Return type capable of getting pids
	return []uint32{}, nil
	//container, err := s.getContainer(id)
	//if err != nil {
	//	return nil, err
	//}
	//p, err := container.Process("")
	//if err != nil {
	//	return nil, errdefs.ToGRPC(err)
	//}
	//ps, err := p.(*proc.Init).Runtime().Ps(ctx, id)
	//if err != nil {
	//	return nil, err
	//}
	//pids := make([]uint32, 0, len(ps))
	//for _, pid := range ps {
	//	pids = append(pids, uint32(pid))
	//}
	//return pids, nil
}

func (s *service) forward(ctx context.Context, publisher shim.Publisher) {
	ns, _ := namespaces.Namespace(ctx)
	ctx = namespaces.WithNamespace(context.Background(), ns)
	for e := range s.events {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := publisher.Publish(ctx, wasmtime.GetTopic(e), e)
		cancel()
		if err != nil {
			logrus.WithError(err).Error("post event")
		}
	}
	publisher.Close()
}

func (s *service) getContainer(id string) (*wasmtime.Container, error) {
	s.mu.Lock()
	container := s.containers[id]
	s.mu.Unlock()
	if container == nil {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "container not created")
	}
	return container, nil
}

// initialize a single epoll fd to manage our consoles. `initPlatform` should
// only be called once.
func (s *service) initPlatform() error {
	if s.platform != nil {
		return nil
	}
	p, err := wasmtime.NewPlatform()
	if err != nil {
		return err
	}
	s.platform = p
	return nil
}
