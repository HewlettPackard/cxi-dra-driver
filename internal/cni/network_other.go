//go:build !linux

package cni

import (
	"errors"

	types100 "github.com/containernetworking/cni/pkg/types/100"
)

// errUnsupported is returned by every linkManager method on non-Linux
// platforms. IPVLAN and network namespaces are Linux-only; this stub exists
// so the package (and its platform-independent tests) build everywhere.
var errUnsupported = errors.New("slingshot-cni: network operations are only supported on linux")

type unsupportedManager struct{}

func newLinkManager() linkManager { return unsupportedManager{} }

func (unsupportedManager) CreateIPVLAN(_, _, _, _ string) (string, error) { return "", errUnsupported }
func (unsupportedManager) Find(_, _ string) (string, error)               { return "", errUnsupported }
func (unsupportedManager) Delete(_, _ string) error                       { return errUnsupported }
func (unsupportedManager) ConfigureAddress(_, _ string, _ *types100.Result) error {
	return errUnsupported
}
func (unsupportedManager) Check(_, _ string) error { return errUnsupported }
