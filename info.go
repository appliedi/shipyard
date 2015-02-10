package shipyard

type (
	ClusterInfo struct {
		Cpus           int64  `json:"cpus,omitempty"`
		Memory         int64  `json:"memory,omitempty"`
		ContainerCount int64  `json:"container_count,omitempty"`
		ImageCount     int64  `json:"image_count,omitempty"`
		Version        string `json:"version,omitempty"`
	}
)
