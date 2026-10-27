#ifndef LODE_TRANSFER_BRIDGE_H
#define LODE_TRANSFER_BRIDGE_H
#include <librsync.h>
#include <stdint.h>
#include <stddef.h>
extern int goRead(uintptr_t, unsigned char *, size_t, size_t *, int *);
extern int goWrite(uintptr_t, unsigned char *, size_t);
extern int goCopy(uintptr_t, rs_long_t, unsigned char *, size_t, size_t *);
rs_job_t *lode_patch_begin(uintptr_t, void **);
void lode_free_copy_context(void *);
rs_result lode_drive(rs_job_t *, uintptr_t, int, size_t *);
#endif
