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

// resource_args bundles the per-service CXI resource limits. Passed from Go
// as a value struct so we avoid pointer aliasing issues across the CGo boundary.
typedef struct {
    int64_t txqs;
    int64_t tgqs;
    int64_t eqs;
    int64_t cts;
    int64_t tles;
    int64_t ptes;
    int64_t les;
    int64_t acs;
} resource_args;

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

// cxi_device_capacity reads hardware resource maxima from the device via
// the cxil_devinfo embedded in struct cxil_dev (populated by cxil_open_device).
static int cxi_device_capacity(const char *device, resource_args *out)
{
    struct cxil_dev *dev = NULL;
    int dev_idx = device_index(device);
    if (dev_idx < 0)
        return -EINVAL;
    int rc = cxil_open_device(dev_idx, &dev);
    if (rc != 0)
        return rc;

    out->txqs = dev->info.num_txqs;
    out->tgqs = dev->info.num_tgqs;
    out->eqs  = dev->info.num_eqs;
    out->cts  = dev->info.num_cts;
    out->tles = dev->info.num_tles;
    out->ptes = dev->info.num_ptes;
    out->les  = dev->info.num_les;
    out->acs  = dev->info.num_acs;

    cxil_close_device(dev);
    return 0;
}

// cxi_sum_reserved calls cxil_get_svc_list and sums the reserved values across
// all services on the device. The result is used by the Go side to compute
// available capacity as hardware_total - sum_reserved, with LE/TLE pool
// rounding applied in Go.
static int cxi_sum_reserved(const char *device, resource_args *out)
{
    struct cxil_dev *dev = NULL;
    int dev_idx = device_index(device);
    if (dev_idx < 0)
        return -EINVAL;
    int rc = cxil_open_device(dev_idx, &dev);
    if (rc != 0)
        return rc;

    struct cxil_svc_list *list = NULL;
    rc = cxil_get_svc_list(dev, &list);
    if (rc != 0) {
        cxil_close_device(dev);
        return rc;
    }

    memset(out, 0, sizeof(*out));
    for (unsigned int i = 0; i < list->count; i++) {
        const struct cxi_svc_desc *d = &list->descs[i];
        if (!d->resource_limits)
            continue;
        out->txqs += d->limits.txqs.res;
        out->tgqs += d->limits.tgqs.res;
        out->eqs  += d->limits.eqs.res;
        out->cts  += d->limits.cts.res;
        out->tles += d->limits.tles.res;
        out->ptes += d->limits.ptes.res;
        out->les  += d->limits.les.res;
        out->acs  += d->limits.acs.res;
    }

    cxil_free_svc_list(list);
    cxil_close_device(dev);
    return 0;
}

// cxi_create_service allocates a CXI service with the VNI and network
// namespace bound at creation time via cxi_svc_desc fields and the
// cxil_svc_set_netns post-creation ioctl respectively.
//
// VNI is set in the descriptor (restricted_vnis=1, num_vld_vnis=1, vnis[0])
// rather than via the post-creation cxil_svc_set_vni_range ioctl. The range
// ioctl was found to cause EEXIST on domain allocation for VNIs occupied at
// the kernel domain level by system components.
//
// restricted_members=1 is set in the descriptor so that cxil_svc_set_netns
// accepts the call. SHS14 requires the service to be disabled before
// cxil_svc_set_netns and re-enabled after.
static int cxi_create_service(const char *device, uint64_t netns_inode, int vni,
                               resource_args res)
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
    desc.resource_limits = 1;
    // restricted_members=1 is required for cxil_svc_set_netns to succeed;
    // without it the kernel returns EEXIST regardless of other parameters.
    desc.restricted_members = 1;
    // restricted_vnis=1 with num_vld_vnis=1 and vnis[0]=vni locks the service
    // to a single VNI at allocation time. This avoids the post-creation
    // cxil_svc_set_vni_range ioctl which was found to cause EEXIST on domain
    // allocation for VNIs that are pre-occupied at the kernel domain level.
    desc.restricted_vnis = 1;
    desc.num_vld_vnis = 1;
    desc.vnis[0] = (uint16_t)vni;

    // Resource limits: max == res for all resources except LEs.
    //
    // For LEs, max must be set to the hardware pool capacity (dev->info.num_les),
    // NOT to les.res. The kernel uses les.max as a normalization denominator and
    // will scale les.res down if les.max < hardware_num_les. All other resource
    // types do not exhibit this behaviour. See libcxi_test_svc.c le_tle test:
    // the test always sets les.max = C_LPE_STS_LIST_ENTRIES_ENTRIES / C_PE_COUNT
    // (i.e. dev->info.num_les), confirming this is the expected API usage.
    // Values validated against available capacity before reaching here.
    desc.limits.txqs.res = (uint16_t)res.txqs; desc.limits.txqs.max = (uint16_t)res.txqs;
    desc.limits.tgqs.res = (uint16_t)res.tgqs; desc.limits.tgqs.max = (uint16_t)res.tgqs;
    desc.limits.eqs.res  = (uint16_t)res.eqs;  desc.limits.eqs.max  = (uint16_t)res.eqs;
    desc.limits.cts.res  = (uint16_t)res.cts;  desc.limits.cts.max  = (uint16_t)res.cts;
    desc.limits.tles.res = (uint16_t)res.tles; desc.limits.tles.max = (uint16_t)res.tles;
    desc.limits.ptes.res = (uint16_t)res.ptes; desc.limits.ptes.max = (uint16_t)res.ptes;
    desc.limits.les.res  = (uint32_t)res.les;  desc.limits.les.max  = (uint32_t)dev->info.num_les;
    desc.limits.acs.res  = (uint16_t)res.acs;  desc.limits.acs.max  = (uint16_t)res.acs;

    struct cxi_svc_fail_info fail_info = {0};
    rc = cxil_alloc_svc(dev, &desc, &fail_info);
    if (rc <= 0) {
        fprintf(stderr, "cxi_wrapper: cxil_alloc_svc failed: %d\n", rc);
        if (rc == 0) rc = -1;
        cxil_close_device(dev);
        return rc;
    }
    int svc_id = rc;

    // SHS14: cxil_svc_set_netns requires the service to be disabled first;
    // re-enable after. See libcxi tests/libcxi_test_svc.c and utils/cxi_service.c.
    rc = cxil_svc_enable(dev, svc_id, false);
    if (rc != 0) {
        fprintf(stderr, "cxi_wrapper: cxil_svc_enable(%d, false) failed: %d\n", svc_id, rc);
        int drc = cxil_destroy_svc(dev, svc_id);
        if (drc != 0)
            fprintf(stderr, "cxi_wrapper: cxil_destroy_svc(%d) in cleanup failed: %d — service may be orphaned\n", svc_id, drc);
        cxil_close_device(dev);
        return rc;
    }

    // SHS14: associate the pod network namespace with this service.
    rc = cxil_svc_set_netns(dev, svc_id, (unsigned int)netns_inode);
    if (rc != 0) {
        fprintf(stderr, "cxi_wrapper: cxil_svc_set_netns(%d, inode=%llu) failed: %d\n",
                svc_id, (unsigned long long)netns_inode, rc);
        cxil_svc_enable(dev, svc_id, true); // best-effort re-enable before destroy
        int drc = cxil_destroy_svc(dev, svc_id);
        if (drc != 0)
            fprintf(stderr, "cxi_wrapper: cxil_destroy_svc(%d) in cleanup failed: %d — service may be orphaned\n", svc_id, drc);
        cxil_close_device(dev);
        return rc;
    }

    rc = cxil_svc_enable(dev, svc_id, true);
    if (rc != 0) {
        fprintf(stderr, "cxi_wrapper: cxil_svc_enable(%d, true) failed: %d\n", svc_id, rc);
        int drc = cxil_destroy_svc(dev, svc_id);
        if (drc != 0)
            fprintf(stderr, "cxi_wrapper: cxil_destroy_svc(%d) in cleanup failed: %d — service may be orphaned\n", svc_id, drc);
        cxil_close_device(dev);
        return rc;
    }

    fprintf(stderr, "cxi_wrapper: created service id=%d on %s inode=%llu vni=%d "
            "txqs=%lld tgqs=%lld eqs=%lld cts=%lld tles=%lld ptes=%lld les=%lld acs=%lld\n",
            svc_id, device, (unsigned long long)netns_inode, vni,
            (long long)res.txqs, (long long)res.tgqs, (long long)res.eqs,
            (long long)res.cts,  (long long)res.tles, (long long)res.ptes,
            (long long)res.les,  (long long)res.acs);
    cxil_close_device(dev);
    return svc_id;
}

// cxi_destroy_service destroys the service identified by svc_id directly via
// cxil_destroy_svc. The svc_id is the kernel-allocated id returned from
// cxi_create_service (cxil_alloc_svc) and tracked per device by the NRI
// plugin, so no service-list scan is required.
static int cxi_destroy_service(const char *device, int svc_id)
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
    rc = cxil_destroy_svc(dev, svc_id);
    if (rc != 0)
        fprintf(stderr, "cxi_wrapper: cxil_destroy_svc(%d) failed: %d\n", svc_id, rc);
    else
        fprintf(stderr, "cxi_wrapper: destroyed service id=%d on %s\n", svc_id, device);
    cxil_close_device(dev);
    return rc;
}

// cxi_list_service_vnis enumerates every service on the device and writes
// each service's assigned VNI into out[0..*count). Only services created with
// restricted_vnis=1 and num_vld_vnis>0 are included; system services and
// services without a VNI restriction are silently skipped — the reconciler
// cannot attribute them to any namespace. The VNI is read directly from
// desc.vnis[0] in the service list returned by cxil_get_svc_list, avoiding
// the need for a separate cxil_svc_get_vni_range ioctl per service.
//
// out must point to a caller-allocated buffer of at least max_out elements.
// On success *count is the number of entries actually written (always
// <= max_out). The DRA reconciler caps max_out at 1024 which is far above
// the kernel's per-device service ceiling.
static int cxi_list_service_vnis(const char *device,
                                  uint16_t *out, unsigned int max_out,
                                  unsigned int *count)
{
    *count = 0;
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
    unsigned int written = 0;
    for (unsigned int i = 0; i < list->count && written < max_out; i++) {
        const struct cxi_svc_desc *d = &list->descs[i];
        // Only services created with restricted_vnis=1 have a meaningful
        // vnis[0]. System services and services without VNI restriction are
        // silently skipped — the reconciler cannot attribute them to any
        // namespace and must not block decisions about attributable slots.
        if (!d->restricted_vnis || d->num_vld_vnis == 0 || d->vnis[0] == 0)
            continue;
        out[written++] = d->vnis[0];
    }
    *count = written;
    cxil_free_svc_list(list);
    cxil_close_device(dev);
    return 0;
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

// ReadDeviceCapacity reads real hardware resource maxima from the device via
// cxil_devinfo embedded in cxil_dev.
func ReadDeviceCapacity(deviceName string) (ResourceSet, error) {
	cDev := C.CString(deviceName)
	defer C.free(unsafe.Pointer(cDev))

	var raw C.resource_args
	rc := C.cxi_device_capacity(cDev, &raw)
	if rc != 0 {
		return ResourceSet{}, fmt.Errorf("cxi_device_capacity(%s) returned %d", deviceName, int(rc))
	}
	return ResourceSet{
		TXQs: int64(raw.txqs),
		TGQs: int64(raw.tgqs),
		EQs:  int64(raw.eqs),
		CTs:  int64(raw.cts),
		TLEs: int64(raw.tles),
		PTEs: int64(raw.ptes),
		LEs:  int64(raw.les),
		ACs:  int64(raw.acs),
	}, nil
}

// readAvailableCapacity polls cxil_get_svc_list, sums reserved values across
// all services, and returns hardware_total minus that sum. LEs and TLEs are
// rounded up to pool boundaries before subtracting, reflecting true pool
// consumption.
func readAvailableCapacity(deviceName string) (ResourceSet, error) {
	total, err := ReadDeviceCapacity(deviceName)
	if err != nil {
		return ResourceSet{}, fmt.Errorf("reading device capacity for %s: %w", deviceName, err)
	}

	cDev := C.CString(deviceName)
	defer C.free(unsafe.Pointer(cDev))

	var raw C.resource_args
	rc := C.cxi_sum_reserved(cDev, &raw)
	if rc != 0 {
		return ResourceSet{}, fmt.Errorf("cxi_sum_reserved(%s) returned %d", deviceName, int(rc))
	}

	reserved := ResourceSet{
		TXQs: int64(raw.txqs),
		TGQs: int64(raw.tgqs),
		EQs:  int64(raw.eqs),
		CTs:  int64(raw.cts),
		TLEs: RoundUpToTLEPool(int64(raw.tles)),
		PTEs: int64(raw.ptes),
		LEs:  RoundUpToLEPool(int64(raw.les)),
		ACs:  int64(raw.acs),
	}

	return total.Sub(reserved), nil
}

func (CXIServiceManager) CreateService(deviceName string, netnsInode uint64, vni int, resources ResourceSet) (int, error) {
	cDev := C.CString(deviceName)
	defer C.free(unsafe.Pointer(cDev))

	res := C.resource_args{
		txqs: C.int64_t(resources.TXQs),
		tgqs: C.int64_t(resources.TGQs),
		eqs:  C.int64_t(resources.EQs),
		cts:  C.int64_t(resources.CTs),
		tles: C.int64_t(resources.TLEs),
		ptes: C.int64_t(resources.PTEs),
		les:  C.int64_t(resources.LEs),
		acs:  C.int64_t(resources.ACs),
	}
	rc := C.cxi_create_service(cDev, C.uint64_t(netnsInode), C.int(vni), res)
	if rc <= 0 {
		return 0, fmt.Errorf("cxi_create_service(%s, inode=%d, vni=%d) returned %d",
			deviceName, netnsInode, vni, int(rc))
	}
	return int(rc), nil
}

func (CXIServiceManager) DeleteService(deviceName string, svcID int) error {
	cDev := C.CString(deviceName)
	defer C.free(unsafe.Pointer(cDev))
	rc := C.cxi_destroy_service(cDev, C.int(svcID))
	if rc != 0 {
		return fmt.Errorf("cxi_destroy_service(%s, svc_id=%d) returned %d",
			deviceName, svcID, int(rc))
	}
	return nil
}

// maxServicesPerDevice bounds the buffer readActiveServiceVNIs hands to the
// C wrapper. The kernel's per-device service ceiling is a few hundred at
// worst, so 1024 is safely above any realistic value while keeping the
// per-call allocation tiny (2 KiB). If a future kernel ever exceeds this the
// reconciler returns an error rather than silently truncating.
const maxServicesPerDevice = 1024

// readActiveServiceVNIs is the CGo-backed implementation of
// ReadActiveServiceVNIs (see capacity_cgo.go). It calls into
// cxi_list_service_vnis which wraps cxil_get_svc_list +
// cxil_svc_get_vni_range. Each returned element is the VNI of one live
// service on the device; the reconciler reverse-maps VNI → namespace via
// the namespace annotation cache to figure out which (namespace, device)
// occupancy slots correspond to real hardware state.
func readActiveServiceVNIs(deviceName string) ([]uint16, error) {
	cDev := C.CString(deviceName)
	defer C.free(unsafe.Pointer(cDev))

	buf := make([]C.uint16_t, maxServicesPerDevice)
	var count C.uint
	rc := C.cxi_list_service_vnis(
		cDev,
		(*C.uint16_t)(unsafe.Pointer(&buf[0])),
		C.uint(maxServicesPerDevice),
		&count,
	)
	if rc != 0 {
		return nil, fmt.Errorf("cxi_list_service_vnis(%s) returned %d", deviceName, int(rc))
	}
	n := int(count)
	if n > maxServicesPerDevice {
		return nil, fmt.Errorf("cxi_list_service_vnis(%s) returned count=%d, exceeds buffer cap %d (raise maxServicesPerDevice)",
			deviceName, n, maxServicesPerDevice)
	}
	out := make([]uint16, n)
	for i := 0; i < n; i++ {
		out[i] = uint16(buf[i])
	}
	return out, nil
}
