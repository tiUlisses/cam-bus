package uplink

import "time"

type Status struct {
	CameraID      string    `json:"cameraId"`
	CentralPath   string    `json:"centralPath"`
	ContainerName string    `json:"containerName"`
	State         string    `json:"state"`
	ExitCode      int       `json:"exitCode"`
	Error         string    `json:"error"`
	Timestamp     time.Time `json:"timestamp"`
}

type StatusHook func(Status)
