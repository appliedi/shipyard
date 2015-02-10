package manager

import (
	"github.com/samalba/dockerclient"
	"github.com/shipyard/shipyard"
)

type (
	EventHandler struct {
		Manager *Manager
	}
)

func (h *EventHandler) Handle(e *dockerclient.Event) error {
	logger.Infof("event: date=%d status=%s container=%s", e.Time, e.Status, e.Id[:12])
	h.logDockerEvent(e)
	return nil
}

func (h *EventHandler) logDockerEvent(e *dockerclient.Event) error {
	evt := &shipyard.Event{
		Type:      e.Status,
		Time:      e.Time,
		Container: e.Id,
		Tags:      []string{"docker"},
	}
	if err := h.Manager.SaveEvent(evt); err != nil {
		return err
	}
	return nil
}
