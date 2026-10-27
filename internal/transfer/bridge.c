#include "bridge.h"
#include <stdlib.h>
#include <string.h>

typedef struct {
    uintptr_t handle;
} copy_ctx;

typedef struct {
    uintptr_t handle;
    unsigned char *in;
    unsigned char *out;
    size_t cap;
} io_ctx;

static rs_result input(rs_job_t *job, rs_buffers_t *b, void *opaque) {
    io_ctx *c = opaque;
    size_t n = 0, kept = b->avail_in;
    int eof = 0;
    (void)job;

    if (b->eof_in || kept > c->cap / 2)
        return RS_DONE;
    if (kept > 0)
        memmove(c->in, b->next_in, kept);
    if (goRead(c->handle, c->in + kept, c->cap - kept, &n, &eof))
        return RS_IO_ERROR;
    b->next_in = c->in;
    b->avail_in = kept + n;
    if (eof)
        b->eof_in = 1;
    return RS_DONE;
}
static rs_result output(rs_job_t *job, rs_buffers_t *b, void *opaque) {
    io_ctx *c = opaque;
    size_t n;
    (void)job;
    if (!b->next_out) {
        b->next_out = c->out;
        b->avail_out = c->cap;
        return RS_DONE;
    }
    n = (size_t)(b->next_out - (char *)c->out);
    if (n > 0 && goWrite(c->handle, c->out, n))
        return RS_IO_ERROR;
    b->next_out = c->out;
    b->avail_out = c->cap;
    return RS_DONE;
}
static rs_result copy(void *opaque, rs_long_t pos, size_t *len, void **buf) {
    copy_ctx *c = opaque;
    size_t n = 0;
    if (goCopy(c->handle, pos, *buf, *len, &n))
        return RS_IO_ERROR;
    *len = n;
    return RS_DONE;
}

rs_job_t *lode_patch_begin(uintptr_t h, void **opaque) {
    copy_ctx *c = malloc(sizeof(*c));
    rs_job_t *j;
    if (!c)
        return NULL;
    c->handle = h;
    j = rs_patch_begin(copy, c);
    if (!j) {
        free(c);
        return NULL;
    }
    *opaque = c;
    return j;
}

void lode_free_copy_context(void *p) {
    free(p);
}

rs_result lode_drive(rs_job_t *j, uintptr_t h, int with_output, size_t *left) {
    const size_t cap = 64 * 1024;
    rs_buffers_t b;
    io_ctx c;
    rs_result r;
    memset(&b, 0, sizeof(b));
    memset(&c, 0, sizeof(c));
    c.handle = h;
    c.cap = cap;
    c.in = malloc(cap);
    c.out = with_output ? malloc(cap) : NULL;
    if (!c.in || (with_output && !c.out)) {
        free(c.in);
        free(c.out);
        return RS_MEM_ERROR;
    }
    r = rs_job_drive(j, &b, input, &c,
                     with_output ? output : NULL,
                     with_output ? &c : NULL);
    *left = b.avail_in;
    free(c.in);
    free(c.out);
    return r;
}

