package shipyard

type (
	Usage struct {
		ID              string `json:"id,omitempty"`
		Version         string `json:"version,omitempty"`
		NumOfImages     int64  `json:"num_of_images,omitempty"`
		NumOfContainers int64  `json:"num_of_containers,omitempty"`
		TotalCpus       int64  `json:"total_cpus,omitempty"`
		TotalMemory     int64  `json:"total_memory,omitempty"`
	}
)
