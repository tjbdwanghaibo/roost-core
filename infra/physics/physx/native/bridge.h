#pragma once
#include <stdint.h>
#ifdef __cplusplus
extern "C" {
#endif
typedef struct rp_world rp_world;
typedef struct {
    uint64_t id;
    float p[3], q[4], v[3], w[3];
    uint32_t sleeping;
} rp_state;
typedef struct {
    uint64_t id;
    float acceleration[3];
    uint32_t jump;
} rp_command;
rp_world *rp_create(float gravity, unsigned workers);
void rp_destroy(rp_world *world);
// kind: 0 box (static), 1 capsule, 2 sphere. dimensions: box half extents; capsule radius/half cylinder
// height; sphere radius.
int rp_add_pose(rp_world *, uint64_t id, unsigned kind, const float *position, const float *rotation, const float *dimensions,
           float mass);
int rp_step_control(rp_world *, float dt, const rp_command *, unsigned count);
unsigned rp_read(rp_world *, rp_state *, unsigned capacity);
const char *rp_version(void);
#ifdef __cplusplus
}
#endif
