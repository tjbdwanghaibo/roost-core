#include "bridge.h"
#include "PxPhysicsAPI.h"
#include <map>
#include <mutex>
#include <new>
#include <vector>
using namespace physx;
// PhysX Foundation 是进程级资源。世界独立，引用计数避免关闭一个世界破坏其他世界。
static std::mutex foundationMutex;
static PxDefaultAllocator allocator;
static PxDefaultErrorCallback errors;
static PxFoundation *foundation = nullptr;
static unsigned users = 0;
struct rp_world {
    PxPhysics *physics = nullptr;
    PxScene *scene = nullptr;
    PxDefaultCpuDispatcher *dispatcher = nullptr;
    PxMaterial *material = nullptr;
    std::map<uint64_t, PxRigidDynamic *> bodies;
    std::vector<PxRigidActor *> actors;
    std::map<uint64_t, PxRigidActor *> all;
    std::map<uint64_t, PxJoint *> joints;
};
// CCD 必须同时启用 scene、body 和接触对；默认 shader 只检测离散接触。
static PxFilterFlags contactFilter(PxFilterObjectAttributes a, PxFilterData ad,
                                   PxFilterObjectAttributes b, PxFilterData bd,
                                   PxPairFlags &pairs, const void *block, PxU32 size) {
    auto result = PxDefaultSimulationFilterShader(a, ad, b, bd, pairs, block, size);
    if (!PxFilterObjectIsTrigger(a) && !PxFilterObjectIsTrigger(b))
        pairs |= PxPairFlag::eDETECT_CCD_CONTACT;
    return result;
}
extern "C" rp_world *rp_create(float gravity, unsigned workers) {
    std::unique_lock<std::mutex> guard(foundationMutex);
    if (!foundation)
        foundation = PxCreateFoundation(PX_PHYSICS_VERSION, allocator, errors);
    if (!foundation)
        return nullptr;
    auto *w = new (std::nothrow) rp_world();
    if (!w) {
        if (!users) {
            foundation->release();
            foundation = nullptr;
        }
        return nullptr;
    }
    w->physics = PxCreatePhysics(PX_PHYSICS_VERSION, *foundation, PxTolerancesScale(), false, nullptr);
    if (!w->physics) {
        delete w;
        if (!users) {
            foundation->release();
            foundation = nullptr;
        }
        return nullptr;
    }
    w->dispatcher = PxDefaultCpuDispatcherCreate(workers);
    PxSceneDesc desc(w->physics->getTolerancesScale());
    desc.gravity = PxVec3(0, gravity, 0);
    desc.cpuDispatcher = w->dispatcher;
    desc.filterShader = contactFilter;
    desc.flags |= PxSceneFlag::eENABLE_CCD;
    w->scene = w->physics->createScene(desc);
    w->material = w->physics->createMaterial(0.6f, 0.6f, 0.1f);
    ++users;
    if (!w->scene || !w->material) {
        guard.unlock();
        rp_destroy(w);
        return nullptr;
    }
    return w;
}
extern "C" void rp_destroy(rp_world *w) {
    if (!w)
        return;
    for (auto &pair : w->joints) pair.second->release();
    // Actor 先于 Scene 释放，避免把移出场景误认为销毁。
    for (auto *actor : w->actors)
        actor->release();
    if (w->scene)
        w->scene->release();
    if (w->material)
        w->material->release();
    if (w->dispatcher)
        w->dispatcher->release();
    // PxCreatePhysics 复用进程单例，其引用计数不是原子的。创建和释放必须共用锁，
    // 但各 Scene 的模拟仍然独立并发，不把 Step 放进此临界区。
    std::lock_guard<std::mutex> guard(foundationMutex);
    if (w->physics)
        w->physics->release();
    delete w;
    if (--users == 0) {
        foundation->release();
        foundation = nullptr;
    }
}
extern "C" int rp_add_pose(rp_world *w, uint64_t id, unsigned kind, unsigned motion, const float *p, const float *q, const float *d, float mass) {
    if (!w || !w->scene || !w->material || w->all.count(id))
        return 0;
    PxTransform pose(PxVec3(p[0], p[1], p[2]), PxQuat(q[0], q[1], q[2], q[3]));
    PxShape *shape = nullptr;
    if (kind == 0)
        shape = w->physics->createShape(PxBoxGeometry(d[0], d[1], d[2]), *w->material, true);
    else if (kind == 1)
        shape = w->physics->createShape(PxCapsuleGeometry(d[0], d[1]), *w->material, true);
    else if (kind == 2)
        shape = w->physics->createShape(PxSphereGeometry(d[0]), *w->material, true);
    if (!shape)
        return 0;
    // PhysX 胶囊沿 X 轴；shape 局部旋转到 Y 轴，actor 的姿态仍与 Unity 对齐。
    if (kind == 1)
        shape->setLocalPose(PxTransform(PxQuat(PxHalfPi, PxVec3(0, 0, 1))));
    if (motion == 1) {
        auto *body = w->physics->createRigidStatic(pose);
        if (!body) {
            shape->release();
            return 0;
        }
        body->attachShape(*shape);
        shape->release();
        w->scene->addActor(*body);
        w->actors.push_back(body);
        w->all[id] = body;
        return 1;
    }
    auto *body = w->physics->createRigidDynamic(pose);
    if (!body) {
        shape->release();
        return 0;
    }
    body->attachShape(*shape);
    shape->release();
    PxRigidBodyExt::setMassAndUpdateInertia(*body, mass);
    body->setLinearDamping(0);
    body->setAngularDamping(0.05f);
    // 显式对齐 Unity 对照场景，不能依赖两个宿主不同的 SDK 默认值。
    body->setMaxAngularVelocity(50.0f);
    body->setMaxDepenetrationVelocity(10.0f);
    body->setSleepThreshold(0.005f);
    body->setSolverIterationCounts(6, 1);
    body->setRigidBodyFlag(motion == 3 ? PxRigidBodyFlag::eKINEMATIC : PxRigidBodyFlag::eENABLE_CCD, true);
    if (kind == 1 && motion != 3) {
        // 禁转胶囊的接触求解也必须禁止转动响应；只锁姿态会低估切向冲量。
        body->setMassSpaceInertiaTensor(PxVec3(0));
        body->setRigidDynamicLockFlags(PxRigidDynamicLockFlag::eLOCK_ANGULAR_X |
                                       PxRigidDynamicLockFlag::eLOCK_ANGULAR_Y |
                                       PxRigidDynamicLockFlag::eLOCK_ANGULAR_Z);
    }
    w->scene->addActor(*body);
    w->actors.push_back(body);
        w->all[id] = body;
    w->bodies[id] = body;
    return 1;
}
// 查询只用于单次跳跃的支撑判定；排除自身，允许站在静态平台或其他动态刚体上。
class GroundFilter : public PxQueryFilterCallback {
    const PxRigidActor *self;
public:
    explicit GroundFilter(const PxRigidActor *body) : self(body) {}
    PxQueryHitType::Enum preFilter(const PxFilterData &, const PxShape *, const PxRigidActor *actor, PxHitFlags &) override {
        return actor == self ? PxQueryHitType::eNONE : PxQueryHitType::eBLOCK;
    }
    PxQueryHitType::Enum postFilter(const PxFilterData &, const PxQueryHit &) override { return PxQueryHitType::eBLOCK; }
};
static void tryJump(rp_world *world, PxRigidDynamic *body) {
    PxShape *shape = nullptr;
    if (body->getShapes(&shape, 1) != 1) return;
    PxReal height = 0;
    PxSphereGeometry sphere;
    PxCapsuleGeometry capsule;
    if (shape->getSphereGeometry(sphere)) height = sphere.radius;
    else if (shape->getCapsuleGeometry(capsule)) height = capsule.radius + capsule.halfHeight;
    else return;
    GroundFilter filter(body);
    PxRaycastBuffer hit;
    PxQueryFilterData data(PxQueryFlag::eSTATIC | PxQueryFlag::eDYNAMIC | PxQueryFlag::ePREFILTER);
    if (world->scene->raycast(body->getGlobalPose().p, PxVec3(0,-1,0), height + 0.1f, hit,
                              PxHitFlag::eDEFAULT, data, &filter) && hit.hasBlock && hit.block.normal.y >= 0.5f) {
        // 起跳检查相对支撑点速度，升降台/转轮接触不能因世界竖直速度大而禁止跳跃。
        PxVec3 supportVelocity(0);
        auto *support = hit.block.actor->is<PxRigidBody>();
        if (support) supportVelocity = PxRigidBodyExt::getVelocityAtPos(*support, hit.block.position);
        if (PxAbs(body->getLinearVelocity().y-supportVelocity.y)<=1.0f)
            body->addForce(PxVec3(0,7,0), PxForceMode::eVELOCITY_CHANGE);
    }
}
extern "C" int rp_step_control(rp_world *w, float dt, const rp_command *commands, unsigned count) {
    if (!w || !w->scene)
        return 0;
    for (unsigned i = 0; i < count; ++i)
        if (!w->bodies.count(commands[i].id))
            return 0;
    for (unsigned i = 0; i < count; ++i) {
        const auto &c = commands[i];
        if (c.target) {
            w->bodies[c.id]->setKinematicTarget(PxTransform(PxVec3(c.p[0],c.p[1],c.p[2]), PxQuat(c.q[0],c.q[1],c.q[2],c.q[3])));
            continue;
        }
        if (c.jump) tryJump(w, w->bodies[c.id]);
        w->bodies[c.id]->addForce(PxVec3(c.acceleration[0], c.acceleration[1], c.acceleration[2]),
                                  PxForceMode::eACCELERATION);
    }
    w->scene->simulate(dt);
    return w->scene->fetchResults(true) ? 1 : 0;
}
extern "C" unsigned rp_read(rp_world *w, rp_state *out, unsigned capacity) {
    if (!w || capacity < w->bodies.size())
        return 0;
    unsigned n = 0;
    for (const auto &pair : w->bodies) {
        auto *b = pair.second;
        auto p = b->getGlobalPose();
        auto v = b->getLinearVelocity();
        auto a = b->getAngularVelocity();
        auto &s = out[n++];
        s.id = pair.first;
        s.p[0] = p.p.x;
        s.p[1] = p.p.y;
        s.p[2] = p.p.z;
        s.q[0] = p.q.x;
        s.q[1] = p.q.y;
        s.q[2] = p.q.z;
        s.q[3] = p.q.w;
        s.v[0] = v.x;
        s.v[1] = v.y;
        s.v[2] = v.z;
        s.w[0] = a.x;
        s.w[1] = a.y;
        s.w[2] = a.z;
        s.sleeping = b->isSleeping();
    }
    return n;
}
extern "C" const char *rp_version() { return "PhysX 4.1.2 / a2c0428acab643e60618c681b501e86f7fd558cc"; }

extern "C" int rp_joint_add(rp_world *w, const rp_joint *j) {
 if (!w || w->joints.count(j->id) || !w->all.count(j->a) || (j->b && !w->all.count(j->b))) return 0;
 PxTransform a(PxVec3(j->pa[0],j->pa[1],j->pa[2]),PxQuat(j->qa[0],j->qa[1],j->qa[2],j->qa[3]));
 PxTransform b(PxVec3(j->pb[0],j->pb[1],j->pb[2]),PxQuat(j->qb[0],j->qb[1],j->qb[2],j->qb[3]));
 auto *ba=w->all[j->a]; auto *bb=j->b?w->all[j->b]:nullptr;
 PxJoint *result=nullptr;
 if (j->kind==0) {
  auto *v=PxDistanceJointCreate(*w->physics,ba,a,bb,b);
  if (!v) return 0;
  v->setMaxDistance(j->distance); v->setStiffness(j->stiffness); v->setDamping(j->damping);
  v->setDistanceJointFlags(PxDistanceJointFlag::eMAX_DISTANCE_ENABLED | PxDistanceJointFlag::eSPRING_ENABLED);
  result=v;
 } else {
  auto *v=PxRevoluteJointCreate(*w->physics,ba,a,bb,b);
  if (!v) return 0;
  v->setDriveVelocity(j->speed); v->setDriveForceLimit(j->force);
  v->setRevoluteJointFlag(PxRevoluteJointFlag::eDRIVE_ENABLED,j->force>0);
  result=v;
 }
 result->setBreakForce(j->break_force,j->break_force);
 w->joints[j->id]=result; return 1;
}
extern "C" void rp_joint_remove(rp_world *w, uint64_t id) {
 if (!w) return;
 auto it=w->joints.find(id); if(it==w->joints.end()) return;
 it->second->release(); w->joints.erase(it);
}

extern "C" unsigned rp_abi_version(void) { return 2; }
