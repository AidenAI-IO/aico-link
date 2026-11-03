/* Exercise production CIF startup, including failures before tasklet enable. */
#include <assert.h>
#include <errno.h>
#include <stdbool.h>
#include <stdint.h>
#include <stddef.h>
#include <stdio.h>
#include <string.h>

typedef uint32_t u32;
enum rkcif_stream_mode { RKCIF_STREAM_MODE_NONE, RKCIF_STREAM_MODE_CAPTURE,
                         RKCIF_STREAM_MODE_TOISP };
enum { RKCIF_STATE_READY, RKCIF_STATE_STREAMING };
enum { NO_HDR, HDR_X2, HDR_X3 };
enum { V4L2_MBUS_CSI2_DPHY, V4L2_MBUS_CSI2_CPHY, V4L2_MBUS_CCP2,
       V4L2_MBUS_BT656, V4L2_MBUS_PARALLEL };
enum { CHIP_RK1808_CIF, CHIP_RV1106_CIF, CHIP_RV1126_CIF,
       CHIP_RV1126_CIF_LITE, CHIP_RK3568_CIF };
#define RKMODULE_START_STREAM_DEFAULT 0
#define RKMODULE_START_STREAM_FRONT 1
#define RKMODULE_MULTI_DEV_COMBINE_ONE 1
#define RKCIF_MAX_CSI_NUM 4
#define RKCIF_SKIP_FRAME_MAX 16
#define RKCIF_STREAM_MIPI_ID0 0
#define RKCIF_STREAM_MIPI_ID1 1
#define RKCIF_STREAM_MIPI_ID2 2
#define VB2_BUF_STATE_QUEUED 1
struct list_head { struct list_head *next, *prev; };
static void list_init(struct list_head *h) { h->next = h->prev = h; }
static void list_add_tail(struct list_head *n, struct list_head *h) {
    n->prev = h->prev; n->next = h; h->prev->next = n; h->prev = n;
}
static void list_del(struct list_head *n) { n->prev->next = n->next; n->next->prev = n->prev; }
#define list_empty(h) ((h)->next == (h))
#define list_first_entry(h, t, m) ((t *)((char *)(h)->next - offsetof(t, m)))
struct tasklet_struct { int count; };
static void tasklet_enable(struct tasklet_struct *t) { assert(t->count > 0); --t->count; }
static void tasklet_disable(struct tasklet_struct *t) { ++t->count; }
#define atomic_read(p) (*(p))
#define atomic_set(p, v) (*(p) = (v))
static void mutex_lock(int *m) { assert(!*m); *m = 1; }
static void mutex_unlock(int *m) { assert(*m); *m = 0; }
struct rkcif_buffer { struct list_head queue; struct { int vb2_buf; } vb; };
struct rkcif_vdev_node { struct { int entity; } vdev; };
struct rkcif_sensor_info {
    void *sd;
    struct { int type; } mbus;
    struct { struct v4l2_fract { unsigned numerator, denominator; } interval; } fi;
};
struct rkmodule_hdr_cfg { int hdr_mode; };
struct rkcif_csi_info { int csi_num, csi_idx[4]; };
struct rkcif_hw { int dev_lock; bool is_rk3588s2; struct { void *vaddr; } dummy_buf; };
struct v4l2_device { int unused; };
struct rkcif_device;
struct rkcif_stream {
    struct rkcif_vdev_node vnode;
    struct rkcif_device *cifdev;
    struct tasklet_struct vb_done_tasklet;
    int id, dma_en, fs_cnt_in_single_frame, state, skip_frame, cur_skip_frame, buf_cnt;
    enum rkcif_stream_mode cur_stream_mode;
    bool is_line_wake_up, is_line_inten;
    void *cif_fmt_in;
    struct rkcif_buffer *curr_buf, *next_buf;
    struct list_head buf_head;
};
struct pipe {
    int stream_cnt, pipe;
    int (*open)(struct pipe *, int *, bool);
    int (*set_stream)(struct pipe *, bool);
};
struct rkcif_device {
    struct rkcif_hw *hw_dev;
    struct v4l2_device v4l2_dev;
    struct rkcif_sensor_info *active_sensor, terminal_sensor;
    struct rkmodule_hdr_cfg hdr;
    struct { struct { int mode; struct { int dev_num, dev_idx[4]; } multi_dev; } capture_info; } channels[1];
    struct pipe pipe;
    struct rkcif_stream stream[3];
    int stream_lock, chip_id, csi_host_idx, csi_host_idx_def, luma_vdev;
    bool is_use_dummybuf, reset_work_cancel;
};
static int format_error, dummy_error, open_error, sensor_error, dma_error, media_error;
static int returned_buffers, delivered_frames;
#define v4l2_info(...) ((void)0)
#define v4l2_err(...) ((void)0)
#define v4l2_subdev_call(...) (-ENOTTY)
#define rkcif_attach_sync_mode(...) ((void)0)
#define rkcif_update_sensor_info(...) (-ENODEV)
#define rkcif_sync_crop_info(...) ((void)0)
#define rkcif_sanity_check_fmt(...) format_error
#define rkcif_create_dummy_buf(...) dummy_error
#define rkcif_csi_stream_start(...) dma_error
#define rkcif_stream_start(...) dma_error
#define media_pipeline_start(...) media_error
#define rkcif_start_luma(...) ((void)0)
#define rkcif_monitor_reset_event(...) ((void)0)
#define rkcif_stream_stop(...) ((void)0)
#define vb2_buffer_done(...) (++returned_buffers)
static int open_pipe(struct pipe *p, int *entity, bool prepare) { return open_error; }
static int set_stream(struct pipe *p, bool on) { return on ? sensor_error : 0; }

/* DRIVER_FUNCTIONS */

static struct rkcif_device dev;
static struct rkcif_hw hw;
static struct rkcif_sensor_info sensor;
static struct rkcif_stream *stream;
static void reset_fixture(void) {
    memset(&dev, 0, sizeof(dev)); memset(&hw, 0, sizeof(hw));
    memset(&sensor, 0, sizeof(sensor));
    dev.hw_dev = &hw; dev.active_sensor = &sensor;
    dev.chip_id = CHIP_RV1106_CIF;
    dev.pipe.open = open_pipe; dev.pipe.set_stream = set_stream;
    stream = &dev.stream[0]; stream->cifdev = &dev;
    stream->vb_done_tasklet.count = 1;
    list_init(&stream->buf_head);
    format_error = dummy_error = open_error = sensor_error = dma_error = media_error = 0;
    returned_buffers = delivered_frames = 0;
}
static void queue_buffer(struct rkcif_buffer *b) {
    memset(b, 0, sizeof(*b)); list_add_tail(&b->queue, &stream->buf_head);
}
static void assert_retry_delivers(void) {
    assert(!dev.stream_lock && !hw.dev_lock);
    assert(stream->vb_done_tasklet.count == 1);
    assert(!rkcif_do_start_stream(stream, RKCIF_STREAM_MODE_CAPTURE));
    assert(!stream->vb_done_tasklet.count);
    /* A scheduled completion can execute only when the tasklet is enabled. */
    if (!stream->vb_done_tasklet.count) ++delivered_frames;
    assert(delivered_frames == 1);
}
int main(void) {
    struct rkcif_buffer buffer;
    reset_fixture();
    for (int i = 0; i < 100; ++i) {
        queue_buffer(&buffer);
        format_error = i & 1 ? -EAGAIN : -ENOLCK;
        assert(rkcif_do_start_stream(stream, RKCIF_STREAM_MODE_CAPTURE) == format_error);
        assert(stream->vb_done_tasklet.count == 1 && list_empty(&stream->buf_head));
    }
    assert(returned_buffers == 100);
    format_error = 0; assert_retry_delivers();
    reset_fixture(); dev.is_use_dummybuf = true; dummy_error = -ENOMEM;
    assert(rkcif_do_start_stream(stream, RKCIF_STREAM_MODE_CAPTURE) == -ENOMEM);
    dummy_error = 0; assert_retry_delivers();
    int *late_errors[] = { &open_error, &sensor_error, &dma_error, &media_error };
    for (unsigned i = 0; i < sizeof(late_errors) / sizeof(late_errors[0]); ++i) {
        reset_fixture(); *late_errors[i] = -EIO;
        assert(rkcif_do_start_stream(stream, RKCIF_STREAM_MODE_CAPTURE) == -EIO);
        *late_errors[i] = 0; assert_retry_delivers();
    }
    reset_fixture();
    assert(!rkcif_do_start_stream(stream, RKCIF_STREAM_MODE_TOISP));
    assert(stream->vb_done_tasklet.count == 1);
    puts("PASS: CIF early and late startup failures preserve tasklet readiness for retry");
    return 0;
}
