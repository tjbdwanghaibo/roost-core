#ifndef ROOST_LOCKSTEP_H
#define ROOST_LOCKSTEP_H
#include "client.h"
#ifdef __cplusplus
extern "C" {
#endif
typedef struct roost_lockstep roost_lockstep;
typedef struct roost_lockstep_input {
  int32_t player;
  const uint8_t *data;
  size_t size;
} roost_lockstep_input;
/* 所有回调同步发生在调用线程，指针只在回调内有效。返回 0 表示失败。
   禁止在回调内销毁/重入同一实例；发送回调只准入网络命令，不等待网络。
   simulate/restore 失败后整场终止，不继续应用部分恢复的状态。 */
typedef struct roost_lockstep_callbacks {
  void *context;
  int (*send)(void *, const uint8_t *, size_t);
  int (*simulate)(void *, uint32_t frame, const roost_lockstep_input *, size_t);
  int (*restore)(void *, uint32_t frame, const uint8_t *, size_t,
                 uint64_t expected_hash, int has_hash, uint64_t *actual_hash);
} roost_lockstep_callbacks;
ROOST_API roost_lockstep *
roost_lockstep_create(roost_lockstep_callbacks callbacks, size_t max_buffered,
                      size_t retry_packets, size_t max_snapshot);
ROOST_API int roost_lockstep_receive(roost_lockstep *, const uint8_t *, size_t);
ROOST_API int roost_lockstep_submit(roost_lockstep *, uint32_t frame,
                                    const uint8_t *, size_t);
ROOST_API int roost_lockstep_hash(roost_lockstep *, uint32_t frame,
                                  uint64_t hash);
ROOST_API int roost_lockstep_catchup(roost_lockstep *, uint32_t frame);
ROOST_API uint64_t roost_lockstep_next(const roost_lockstep *);
/* 错误文本到下一次调用/销毁前有效；返回空串表示尚无错误。 */
ROOST_API const char *roost_lockstep_error(const roost_lockstep *);
ROOST_API void roost_lockstep_destroy(roost_lockstep *);
#ifdef __cplusplus
}
#endif
#endif
