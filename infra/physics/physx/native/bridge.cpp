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
extern "C" int rp_add_pose(rp_world *w, uint64_t id, unsigned kind, const float *p, const float *q, const float *d, float mass) {
    if (!w || !w->scene || !w->material || w->bodies.count(id))
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
    if (kind == 0) {
        auto *body = w->physics->createRigidStatic(pose);
        if (!body) {
            shape->release();
            return 0;
        }
        body->attachShape(*shape);
        shape->release();
        w->scene->addActor(*body);
        w->actors.push_back(body);
        return 1;
    }
    // PhysX 胶囊沿 X 轴；shape 局部旋转到 Y 轴，actor 的姿态仍与 Unity 对齐。
    if (kind == 1)
        shape->setLocalPose(PxTransform(PxQuat(PxHalfPi, PxVec3(0, 0, 1))));
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
    body->setRigidBodyFlag(PxRigidBodyFlag::eENABLE_CCD, true);
    if (kind == 1) {
        // 禁转胶囊的接触求解也必须禁止转动响应；只锁姿态会低估切向冲量。
        body->setMassSpaceInertiaTensor(PxVec3(0));
        body->setRigidDynamicLockFlags(PxRigidDynamicLockFlag::eLOCK_ANGULAR_X |
                                       PxRigidDynamicLockFlag::eLOCK_ANGULAR_Y |
                                       PxRigidDynamicLockFlag::eLOCK_ANGULAR_Z);
    }
    w->scene->addActor(*body);
    w->actors.push_back(body);
    w->bodies[id] = body;
    return 1;
}
extern "C" int rp_step(rp_world *w, float dt, const rp_command *commands, unsigned count) {
    if (!w || !w->scene)
        return 0;
    for (unsigned i = 0; i < count; ++i)
        if (!w->bodies.count(commands[i].id))
            return 0;
    for (unsigned i = 0; i < count; ++i) {
        const auto &c = commands[i];
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
