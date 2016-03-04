package libcontainerd

import (
	"fmt"
	"path/filepath"
	"sync"

	"github.com/Sirupsen/logrus"
	containerd "github.com/docker/containerd/api/grpc/types"
	"golang.org/x/net/context"
)

// Client privides access to containerd features.
type Client interface {
	Create(id string, spec Spec, options ...CreateOption) error
	Signal(id string, sig int) error
	AddProcess(id, processID string, process Process) error
	Resize(id, processID string, width, height int) error
	Pause(id string) error
	Resume(id string) error
	Restore(id string, options ...CreateOption) error
	Stats(id string) (*Stats, error)
	GetPidsForContainer(id string) ([]int, error)
}

type client struct {
	sync.Mutex                              // lock for containerMutexes map access
	mapMutex         sync.RWMutex           // protects read/write oprations from containers map
	containerMutexes map[string]*sync.Mutex // lock by container ID
	backend          Backend
	remote           *remote
	containers       map[string]*container
	q                queue
}

func (c *client) Signal(id string, sig int) error {
	c.lock(id)
	defer c.unlock(id)
	if _, err := c.getContainer(id); err != nil {
		return err
	}
	_, err := c.remote.apiClient.Signal(context.Background(), &containerd.SignalRequest{
		Id:     id,
		Pid:    initProcessID,
		Signal: uint32(sig),
	})
	return err
}

func (c *client) restore(cont *containerd.Container, options ...CreateOption) (err error) {
	c.lock(cont.Id)
	defer c.unlock(cont.Id)

	logrus.Debugf("restore container %s state %s", cont.Id, cont.Status)

	id := cont.Id
	if _, err := c.getContainer(id); err == nil {
		return fmt.Errorf("container %s is aleady active", id)
	}

	defer func() {
		if err != nil {
			c.deleteContainer(cont.Id)
		}
	}()

	container := c.newContainer(cont.BundlePath, options...)
	container.systemPid = systemPid(cont)

	iopipe, err := container.openFifos()
	if err != nil {
		return err
	}

	if err := c.backend.AttachStreams(id, *iopipe); err != nil {
		return err
	}

	c.appendContainer(container)

	err = c.backend.StateChanged(id, StateInfo{
		State: StateRestore,
		Pid:   container.systemPid,
	})

	if err != nil {
		return err
	}

	if event, ok := c.remote.pastEvents[id]; ok {
		// This should only be a pause or resume event
		if event.Type == StatePause || event.Type == StateResume {
			return c.backend.StateChanged(id, StateInfo{
				State: event.Type,
				Pid:   container.systemPid,
			})
		}

		logrus.Warnf("unexpected backlog event: %#v", event)
	}

	return nil
}

func (c *client) Resize(id, processID string, width, height int) error {
	c.lock(id)
	defer c.unlock(id)
	if _, err := c.getContainer(id); err != nil {
		return err
	}
	_, err := c.remote.apiClient.UpdateProcess(context.Background(), &containerd.UpdateProcessRequest{
		Id:     id,
		Pid:    processID,
		Width:  uint32(width),
		Height: uint32(height),
	})
	return err
}

func (c *client) Pause(id string) error {
	return c.setState(id, StatePause)
}

func (c *client) setState(id, state string) error {
	c.lock(id)
	container, err := c.getContainer(id)
	if err != nil {
		c.unlock(id)
		return err
	}
	if container.systemPid == 0 {
		c.unlock(id)
		return fmt.Errorf("No active process for container %s", id)
	}
	st := "running"
	if state == StatePause {
		st = "paused"
	}
	chstate := make(chan struct{})
	_, err = c.remote.apiClient.UpdateContainer(context.Background(), &containerd.UpdateContainerRequest{
		Id:     id,
		Pid:    initProcessID,
		Status: st,
	})
	if err != nil {
		c.unlock(id)
		return err
	}
	container.pauseMonitor.append(state, chstate)
	c.unlock(id)
	<-chstate
	return nil
}

func (c *client) Resume(id string) error {
	return c.setState(id, StateResume)
}

func (c *client) Stats(id string) (*Stats, error) {
	resp, err := c.remote.apiClient.Stats(context.Background(), &containerd.StatsRequest{id})
	if err != nil {
		return nil, err
	}
	return (*Stats)(resp), nil
}

func (c *client) Restore(id string, options ...CreateOption) error {
	cont, err := c.getContainerdContainer(id)
	if err == nil {
		if err := c.restore(cont, options...); err != nil {
			logrus.Errorf("error restoring %s: %v", id, err)
		}
		return nil
	}
	c.lock(id)
	defer c.unlock(id)

	var exitCode uint32
	if event, ok := c.remote.pastEvents[id]; ok {
		exitCode = event.Status
		delete(c.remote.pastEvents, id)
	}

	return c.backend.StateChanged(id, StateInfo{
		State:    StateExit,
		ExitCode: exitCode,
	})
}

func (c *client) GetPidsForContainer(id string) ([]int, error) {
	cont, err := c.getContainerdContainer(id)
	if err != nil {
		return nil, err
	}
	pids := make([]int, len(cont.Pids))
	for i, p := range cont.Pids {
		pids[i] = int(p)
	}
	return pids, nil
}

func (c *client) getContainerdContainer(id string) (*containerd.Container, error) {
	resp, err := c.remote.apiClient.State(context.Background(), &containerd.StateRequest{Id: id})
	if err != nil {
		return nil, err
	}
	for _, cont := range resp.Containers {
		if cont.Id == id {
			return cont, nil
		}
	}
	return nil, fmt.Errorf("invalid state response")
}

func (c *client) newContainer(dir string, options ...CreateOption) *container {
	container := &container{
		process: process{
			id:        filepath.Base(dir),
			dir:       dir,
			client:    c,
			processID: initProcessID,
		},
		processes: make(map[string]*process),
	}
	for _, option := range options {
		if err := option.Apply(container); err != nil {
			logrus.Error(err)
		}
	}
	return container
}

func (c *client) getContainer(id string) (*container, error) {
	c.mapMutex.RLock()
	container, ok := c.containers[id]
	defer c.mapMutex.RUnlock()
	if !ok {
		return nil, fmt.Errorf("invalid container: %s", id) // fixme: typed error
	}
	return container, nil
}

func (c *client) lock(id string) {
	c.Lock()
	if _, ok := c.containerMutexes[id]; !ok {
		c.containerMutexes[id] = &sync.Mutex{}
	}
	c.Unlock()
	c.containerMutexes[id].Lock()
}

func (c *client) unlock(id string) {
	c.Lock()
	if l, ok := c.containerMutexes[id]; ok {
		l.Unlock()
	} else {
		logrus.Warnf("unlock of non-existing mutex: %s", id)
	}
	c.Unlock()
}

// must hold a lock for c.ID
func (c *client) appendContainer(cont *container) {
	c.mapMutex.Lock()
	c.containers[cont.id] = cont
	c.mapMutex.Unlock()
}
func (c *client) deleteContainer(id string) {
	c.mapMutex.Lock()
	delete(c.containers, id)
	c.mapMutex.Unlock()
}
