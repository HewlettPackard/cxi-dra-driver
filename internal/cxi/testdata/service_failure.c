#include <assert.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <errno.h>

struct limit { uint32_t res, max; };
struct cxil_dev { struct { uint32_t num_les; } info; };
struct cxi_svc_desc {
    int resource_limits, restricted_members, restricted_vnis, num_vld_vnis;
    uint16_t vnis[4];
    struct {
        struct limit txqs, tgqs, eqs, cts, tles, ptes, les, acs;
    } limits;
};
struct cxi_svc_fail_info { int unused; };

static const char *failure;
static int destroy_fails, open_count, close_count, alloc_count;
static int disable_count, netns_count, enable_count, destroy_count;
static int live_services, reserved_txqs;
static int vf_mode;
static struct cxil_dev device = {{16384}};

static int device_index(const char *name)
{
    assert(strcmp(name, "cxi4") == 0);
    return 4;
}

static int cxil_open_device(int index, struct cxil_dev **out)
{
    assert(index == 4);
    open_count++;
    if (strcmp(failure, "open device") == 0)
        return -EPERM;
    *out = &device;
    return 0;
}

static void cxil_close_device(struct cxil_dev *dev)
{
    assert(dev == &device);
    close_count++;
}

static int cxil_alloc_svc(struct cxil_dev *dev, struct cxi_svc_desc *desc,
                          struct cxi_svc_fail_info *info)
{
    assert(dev == &device && info != NULL);
    assert(desc->restricted_members && desc->restricted_vnis);
    assert(desc->num_vld_vnis == 1 && desc->vnis[0] == 33);
    assert(desc->limits.txqs.res == 1 && desc->limits.txqs.max == 1);
    assert(desc->limits.les.res == 0 && desc->limits.tles.res == 0);
    assert(desc->limits.les.max == (vf_mode ? 8192u : 16384u));
    alloc_count++;
    if (strcmp(failure, "allocate") == 0)
        return -ENOSPC;
    live_services++;
    reserved_txqs += desc->limits.txqs.res;
    return 7;
}

static int cxil_svc_enable(struct cxil_dev *dev, int id, bool enabled)
{
    assert(dev == &device && id == 7 && live_services == 1);
    if (enabled) {
        assert(disable_count == 1 && netns_count == 1);
        enable_count++;
        if (strcmp(failure, "enable") == 0)
            return -EINVAL;
    } else {
        assert(netns_count == 0 && enable_count == 0);
        disable_count++;
        if (strcmp(failure, "disable") == 0)
            return -EPERM;
    }
    return 0;
}

static int cxil_svc_set_netns(struct cxil_dev *dev, int id, unsigned int inode)
{
    assert(dev == &device && id == 7 && inode == 123 && live_services == 1);
    assert(disable_count == 1 && enable_count == 0);
    netns_count++;
    return strcmp(failure, "set netns") == 0 ? -EINVAL : 0;
}

static int cxil_destroy_svc(struct cxil_dev *dev, int id)
{
    assert(dev == &device && id == 7 && live_services == 1);
    destroy_count++;
    if (destroy_fails)
        return -EBUSY;
    live_services--;
    reserved_txqs = 0;
    return 0;
}

#include "production_create.h"

int main(int argc, char **argv)
{
    assert(argc == 4);
    failure = argv[1];
    destroy_fails = strcmp(argv[2], "fail") == 0;
    vf_mode = strcmp(argv[3], "vf") == 0;
    const char *operation = NULL;
    resource_args resources = {0};
    resources.txqs = 1;
    int rc = cxi_create_service("cxi4", 123, 33, resources,
                               vf_mode, 8192, &operation);
    assert(open_count == 1);
    if (strcmp(failure, "success") == 0) {
        assert(rc == 7 && live_services == 1 && reserved_txqs == 1);
        assert(disable_count == 1 && netns_count == 1 && enable_count == 1);
        assert(destroy_count == 0 && close_count == 1);
        assert(cxil_destroy_svc(&device, 7) == 0);
        return 0;
    }
    assert(strcmp(operation, failure) == 0);
    if (strcmp(failure, "open device") == 0) {
        assert(rc == -EPERM && alloc_count == 0 && close_count == 0);
        assert(destroy_count == 0 && live_services == 0);
    } else if (strcmp(failure, "allocate") == 0) {
        assert(rc == -ENOSPC && alloc_count == 1 && close_count == 1);
        assert(disable_count == 0 && destroy_count == 0 && live_services == 0);
    } else {
        assert(rc == (strcmp(failure, "disable") == 0 ? -EPERM : -EINVAL));
        assert(close_count == 1 && destroy_count == 1 && disable_count == 1);
        assert(netns_count == (strcmp(failure, "disable") != 0));
        assert(enable_count == (strcmp(failure, "disable") != 0));
        assert(live_services == (destroy_fails ? 1 : 0));
        assert(reserved_txqs == (destroy_fails ? 1 : 0));
    }
    return 0;
}
