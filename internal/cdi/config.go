package cdi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
)

// LoadConfig reads and JSON-decodes a CDIEnvConfig from the given file.
// The file is mounted into the driver pod from a ConfigMap so that library
// paths can change without rebuilding the binary.
func LoadConfig(file string) (CDIEnvConfig, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return CDIEnvConfig{}, fmt.Errorf("reading CDI env config %s: %w", file, err)
	}
	var cfg CDIEnvConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return CDIEnvConfig{}, fmt.Errorf("parsing CDI env config %s: %w", file, err)
	}
	if err := cfg.Validate(); err != nil {
		return CDIEnvConfig{}, fmt.Errorf("CDI env config %s: %w", file, err)
	}
	return cfg, nil
}

// Validate checks path shapes and that no two libs land on the same
// /host/env file name.
func (c CDIEnvConfig) Validate() error {
	if len(c.Libs) == 0 {
		return fmt.Errorf("libs is empty")
	}
	names := make(map[string]string, len(c.Libs))
	for _, l := range c.Libs {
		if !filepath.IsAbs(l) || filepath.Base(l) == "/" {
			return fmt.Errorf("libs: %q must be an absolute file path", l)
		}
		name := filepath.Base(l)
		if prev, dup := names[name]; dup {
			return fmt.Errorf("libs: %q and %q both mount at %s", prev, l, path.Join(EnvHostDir, name))
		}
		names[name] = l
	}
	return nil
}
