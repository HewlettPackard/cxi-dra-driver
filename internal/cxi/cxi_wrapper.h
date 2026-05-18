/* cxi_wrapper.h — thin shim around libcxi service management ioctls.
 *
 * Only compiled with the "cxi" build tag; requires:
 *   /usr/include/libcxi/libcxi.h
 *   /usr/lib64/libcxi.so  (or .a for static link)
 *
 * The API mirrors the caps-tum patch approach:
 *   - create: open device, fill cxi_svc_desc with VNI + netns member, ioctl
 *   - delete: open device, call cxil_get_svc_list, find matching netns inode, destroy
 */
#ifndef CXI_WRAPPER_H
#define CXI_WRAPPER_H

#include <stdint.h>

/* Creates a CXI service on `device` (e.g. "cxi0") scoped to the network
 * namespace identified by `netns_inode` using fabric VNI `vni`.
 * Returns 0 on success, -errno on failure.                                  */
int cxi_create_service(const char *device, uint64_t netns_inode, int vni);

/* Finds the CXI service on `device` whose netns member matches `netns_inode`
 * and destroys it.
 * Returns 0 on success, -errno on failure, -ENOENT if not found.            */
int cxi_delete_service(const char *device, uint64_t netns_inode);

#endif /* CXI_WRAPPER_H */
