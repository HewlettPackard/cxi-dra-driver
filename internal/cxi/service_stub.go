//go:build !cxi
package cxi
import "fmt"
type StubServiceManager struct{}
var _ ServiceManager = StubServiceManager{}
func (StubServiceManager) CreateService(deviceName string, netnsInode uint64, vni int) (int, error) {
	return 0, fmt.Errorf("cxi: CreateService not implemented (rebuild with -tags cxi and libcxi installed at /opt/libcxi-netns/)")
}
func (StubServiceManager) DeleteService(deviceName string, netnsInode uint64) error {
	return fmt.Errorf("cxi: DeleteService not implemented (rebuild with -tags cxi and libcxi installed at /opt/libcxi-netns/)")
}
func NewServiceManager() ServiceManager {
	return StubServiceManager{}
}
