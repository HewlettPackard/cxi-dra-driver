//go:build cxi
package cxi
/*
#cgo CFLAGS:  -I/usr/include
#cgo LDFLAGS: -L/usr/lib64 -lcxi
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <libcxi/libcxi.h>
static int device_index(const char *name)
{
    int idx = 0;
    const char *p = name;
    while (*p && (*p < '0' || *p > '9'))
        p++;
    if (!*p)
        return -1;
    while (*p >= '0' && *p <= '9')
        idx = idx * 10 + (*p++ - '0');
    return idx;
}
static int cxi_create_service(const char *device, uint64_t netns_inode, int vni)
{
    struct cxil_dev *dev = NULL;
    int dev_idx = device_index(device);
    if (dev_idx < 0)
        return -EINVAL;
    int rc = cxil_open_device(dev_idx, &dev);
    if (rc != 0) {
        fprintf(stderr, "cxi_wrapper: cxil_open_device(%d) failed: %d\n", dev_idx, rc);
        return rc;
    }
    struct cxi_svc_desc desc = {0};
    desc.resource_limits    = 1;
    desc.restricted_members = 1;
    desc.restricted_vnis    = 1;
    desc.limits.txqs.res = 2;   desc.limits.txqs.max = 1024;
    desc.limits.tgqs.res = 1;   desc.limits.tgqs.max = 512;
    desc.limits.eqs.res  = 2;   desc.limits.eqs.max  = 2047;
    desc.limits.cts.res  = 1;   desc.limits.cts.max  = 2047;
    desc.limits.tles.res = 1;   desc.limits.tles.max = 2048;
    desc.limits.ptes.res = 6;   desc.limits.ptes.max = 2048;
    desc.limits.les.res  = 1;   desc.limits.les.max  = 16384;
    desc.limits.acs.res  = 4;   desc.limits.acs.max  = 1022;
    desc.num_vld_vnis = 1;
    desc.vnis[0]      = (uint16_t)vni;
    desc.members[0].type             = CXI_SVC_MEMBER_NET_NS;
    desc.members[0].svc_member.netns = (unsigned int)netns_inode;
    struct cxi_svc_fail_info fail_info = {0};
    rc = cxil_alloc_svc(dev, &desc, &fail_info);
    if (rc <= 0) {
        fprintf(stderr, "cxi_wrapper: cxil_alloc_svc failed: %d\n", rc);
        if (rc == 0) rc = -1;
    } else {
        fprintf(stderr, "cxi_wrapper: created service id=%d on %s inode=%llu vni=%d\n",
                rc, device, (unsigned long long)netns_inode, vni);
    }
    cxil_close_device(dev);
    return rc;
}
static int cxi_delete_service(const char *device, uint64_t netns_inode)
{
    struct cxil_dev *dev = NULL;
    int dev_idx = device_index(device);
    if (dev_idx < 0)
        return -EINVAL;
    int rc = cxil_open_device(dev_idx, &dev);
    if (rc != 0) {
        fprintf(stderr, "cxi_wrapper: cxil_open_device(%d) failed: %d\n", dev_idx, rc);
        return rc;
    }
    struct cxil_svc_list *list = NULL;
    rc = cxil_get_svc_list(dev, &list);
    if (rc != 0) {
        fprintf(stderr, "cxi_wrapper: cxil_get_svc_list failed: %d\n", rc);
        cxil_close_device(dev);
        return rc;
    }
    unsigned int target_id = 0;
    for (unsigned int i = 0; i < list->count; i++) {
        const struct cxi_svc_desc *d = &list->descs[i];
        for (unsigned int m = 0; m < CXI_SVC_MAX_MEMBERS; m++) {
            if (d->members[m].type == CXI_SVC_MEMBER_NET_NS &&
                d->members[m].svc_member.netns == (unsigned int)netns_inode) {
                target_id = d->svc_id;
                break;
            }
        }
        if (target_id)
            break;
    }
    cxil_free_svc_list(list);
    if (!target_id) {
        fprintf(stderr, "cxi_wrapper: no service found for inode=%llu on %s\n",
                (unsigned long long)netns_inode, device);
        cxil_close_device(dev);
        return -ENOENT;
    }
    rc = cxil_destroy_svc(dev, target_id);
    if (rc != 0)
        fprintf(stderr, "cxi_wrapper: cxil_destroy_svc(%u) failed: %d\n", target_id, rc);
    else
        fprintf(stderr, "cxi_wrapper: destroyed service id=%u on %s\n", target_id, device);
    cxil_close_device(dev);
    return rc;
}
*/
import "C"
import (
"fmt"
"unsafe"
)
// CXIServiceManager is the real ServiceManager backed by libcxi ioctls.
type CXIServiceManager struct{}
var _ ServiceManager = CXIServiceManager{}
// NewServiceManager returns the CGo-backed implementation.
func NewServiceManager() ServiceManager {
return CXIServiceManager{}
}
func (CXIServiceManager) CreateService(deviceName string, netnsInode uint64, vni int) (int, error) {
cDev := C.CString(deviceName)
defer C.free(unsafe.Pointer(cDev))
rc := C.cxi_create_service(cDev, C.uint64_t(netnsInode), C.int(vni))
if rc <= 0 {
return 0, fmt.Errorf("cxi_create_service(%s, inode=%d, vni=%d) returned %d",
deviceName, netnsInode, vni, int(rc))
}
return int(rc), nil
}
func (CXIServiceManager) DeleteService(deviceName string, netnsInode uint64) error {
cDev := C.CString(deviceName)
defer C.free(unsafe.Pointer(cDev))
rc := C.cxi_delete_service(cDev, C.uint64_t(netnsInode))
if rc != 0 {
return fmt.Errorf("cxi_delete_service(%s, inode=%d) returned %d",
deviceName, netnsInode, int(rc))
}
return nil
}
