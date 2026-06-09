package cdi

import "fmt"

// CDIEnvConfig holds configurable paths for environment CDI spec generation.
// XpmemPath and GdrDrvPath default to the standard paths when empty.
type CDIEnvConfig struct {
	LibfabricLib string // host path to libfabric.so.1; empty = skip mount

	NCCLPrefix string // Spack NCCL install dir; empty = skip NCCL spec

	AWSofiNCCLPrefix string // Spack aws-ofi-nccl install dir; empty = skip OFI spec
	HwlocLib         string
	PciaccessLib     string
	Libxml2Lib       string
	LibatomicLib     string
	LiblzmaLib       string
	LibiconvLib      string

	// Optional path overrides for /dev/xpmem and /dev/gdrdrv; used in tests.
	XpmemPath  string
	GdrDrvPath string
}

func EnvDeviceID(driverName string) string {
	return fmt.Sprintf("%s/env=env", driverName)
}

func NCCLDeviceID(driverName string) string {
	return fmt.Sprintf("%s/nccl=nccl", driverName)
}

func AWSofiNCCLDeviceID(driverName string) string {
	return fmt.Sprintf("%s/aws-ofi-nccl=aws-ofi-nccl", driverName)
}
