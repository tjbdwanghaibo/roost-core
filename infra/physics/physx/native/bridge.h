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
    uint32_t jump, target;
    float p[3], q[4];
} rp_command;
rp_world *rp_create(float gravity, unsigned workers);
void rp_destroy(rp_world *world);
// kind: 0 box, 1 capsule, 2 sphere. motion: 1 static, 2 dynamic, 3 kinematic. dimensions: box half extents; capsule radius/half cylinder
// height; sphere radius.
int rp_add_pose(rp_world *, uint64_t id, unsigned kind, unsigned motion, const float *position, const float *rotation, const float *dimensions,
           float mass);
int rp_step_control(rp_world *, float dt, const rp_command *, unsigned count);
unsigned rp_read(rp_world *, rp_state *, unsigned capacity);
typedef struct {
 uint64_t id, a, b;
 unsigned kind;
 float pa[3], pb[3], qa[4], qb[4];
 float distance, stiffness, damping, speed, force, break_force;
} rp_joint;
int rp_joint_add(rp_world *, const rp_joint *);
void rp_joint_remove(rp_world *, uint64_t id);
unsigned rp_abi_version(void);
const char *rp_version(void);
#ifdef __cplusplus
}
#endif
