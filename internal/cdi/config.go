package cdi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// LoadConfig reads and JSON-decodes a CDIEnvConfig from the given file.
// The file is mounted into the driver pod from a ConfigMap so that library
// paths can change without rebuilding the binary.
func LoadConfig(path string) (CDIEnvConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return CDIEnvConfig{}, fmt.Errorf("reading CDI env config %s: %w", path, err)
	}
	var cfg CDIEnvConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return CDIEnvConfig{}, fmt.Errorf("parsing CDI env config %s: %w", path, err)
	}
	if len(cfg.EnvMounts) == 0 {
		return CDIEnvConfig{}, fmt.Errorf("CDI env config %s has no envMounts entries", path)
	}
	for i, m := range cfg.EnvMounts {
		if m.HostPath == "" || m.ContainerPath == "" {
			return CDIEnvConfig{}, fmt.Errorf("CDI env config %s envMounts[%d]: hostPath and containerPath are required", path, i)
		}
	}
	for i, m := range cfg.AWSofiMounts {
		if m.HostPath == "" || m.ContainerPath == "" {
			return CDIEnvConfig{}, fmt.Errorf("CDI env config %s awsOfiMounts[%d]: hostPath and containerPath are required", path, i)
		}
	}
	return cfg, nil
}
