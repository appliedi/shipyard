package shipyard

type Event struct {
	Type      string   `json:"type,omitempty"`
	Container string   `json:"container,omitempty"`
	Time      int64    `json:"time,omitempty"`
	Message   string   `json:"message,omitempty"`
	Tags      []string `json:"tags,omitempty"`
}
