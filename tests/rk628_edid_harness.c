/* Exercise the production EDID reset/get wrappers with competing readers. */
#include <assert.h>
#include <errno.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>

typedef uint8_t u8;
#define EDID_BLOCK_SIZE 128
#define ARRAY_SIZE(a) (sizeof(a) / sizeof((a)[0]))
#define msecs_to_jiffies(ms) (ms)

struct v4l2_subdev { int unused; };
struct v4l2_subdev_edid {
    unsigned int pad, start_block, blocks;
    u8 *edid;
};
struct rk628_csi {
    struct v4l2_subdev sd;
    int confctl_mutex, delayed_work_enable_hotplug;
    u8 edid_version;
};
static struct rk628_csi csi;
static u8 edid_init_data[256], hardware_edid[256], result[256];
static unsigned int reads, writes, plugouts, schedules, rejected_reads;
static int read_error;

static struct rk628_csi *to_csi(struct v4l2_subdev *sd)
{
    assert(sd == &csi.sd);
    return &csi;
}
static bool mutex_trylock(int *lock)
{
    if (*lock)
        return false;
    *lock = 1;
    return true;
}
static void mutex_lock(int *lock)
{
    bool locked = mutex_trylock(lock);
    assert(locked);
}
static void mutex_unlock(int *lock)
{
    assert(*lock);
    *lock = 0;
}

static int rk628_csi_get_edid(struct v4l2_subdev *, struct v4l2_subdev_edid *);

static void competing_read(void)
{
    struct v4l2_subdev_edid edid = { .blocks = 2, .edid = result };
    /* A read interleaved with reset must fail before accessing the hardware. */
    unsigned int before = reads;
    assert(rk628_csi_get_edid(&csi.sd, &edid) == -EAGAIN);
    assert(reads == before);
    ++rejected_reads;
}
static void rk628_hdmirx_plugout(struct v4l2_subdev *sd)
{
    assert(to_csi(sd)->confctl_mutex);
    ++plugouts;
    competing_read();
}
static int rk628_csi_s_edid(struct v4l2_subdev *sd, struct v4l2_subdev_edid *edid)
{
    assert(to_csi(sd)->confctl_mutex);
    assert(!edid->pad && !edid->start_block && edid->blocks == 2);
    ++writes;
    memcpy(hardware_edid, edid->edid, EDID_BLOCK_SIZE);
    competing_read();
    memcpy(hardware_edid + EDID_BLOCK_SIZE, edid->edid + EDID_BLOCK_SIZE,
           EDID_BLOCK_SIZE);
    return 0;
}
static int rk628_csi_g_edid(struct v4l2_subdev *sd, struct v4l2_subdev_edid *edid)
{
    assert(to_csi(sd)->confctl_mutex);
    ++reads;
    if (read_error)
        return read_error;
    memcpy(edid->edid, hardware_edid, sizeof(hardware_edid));
    return 0;
}
static void schedule_delayed_work(int *work, unsigned int delay)
{
    assert(!csi.confctl_mutex);
    assert(work == &csi.delayed_work_enable_hotplug && delay == 1000);
    ++schedules;
}

/* DRIVER_FUNCTIONS */

int main(void)
{
    struct v4l2_subdev_edid edid = { .blocks = 2, .edid = result };
    u8 profiles[2][256];
    memset(profiles[0], 0x30, sizeof(profiles[0]));
    memset(profiles[1], 0x60, sizeof(profiles[1]));

    for (unsigned int i = 0; i < 2; ++i) {
        rk628_reset_edid(&csi.sd, profiles[i], i + 1);
        assert(!csi.confctl_mutex && csi.edid_version == i + 1);
        assert(rk628_csi_get_edid(&csi.sd, &edid) == 0);
        assert(!memcmp(result, profiles[i], sizeof(result)));
        assert(!csi.confctl_mutex);
    }
    assert(plugouts == 2 && writes == 2 && schedules == 2);
    assert(reads == 2 && rejected_reads == 4);

    /* A failed hardware read must release the mutex and allow reset/retry. */
    read_error = -EIO;
    assert(rk628_csi_get_edid(&csi.sd, &edid) == -EIO);
    assert(!csi.confctl_mutex);
    rk628_reset_edid(&csi.sd, profiles[0], 1);
    read_error = 0;
    assert(rk628_csi_get_edid(&csi.sd, &edid) == 0);
    assert(!memcmp(result, profiles[0], sizeof(result)));
    assert(!csi.confctl_mutex);
    puts("PASS: EDID reset excludes readers until the complete profile is written");
    return 0;
}
