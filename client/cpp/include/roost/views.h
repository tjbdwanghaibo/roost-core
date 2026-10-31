#ifndef ROOST_VIEWS_H
#define ROOST_VIEWS_H
#include "client.h"
#ifdef __cplusplus
extern "C" {
#endif
/* Optional ROOST_WITH_PROTOBUF library API. All operations run on the consumer
   thread. Descriptor set is produced by protoc --include_imports. */
typedef struct roost_views roost_views;
typedef struct roost_view_change {
  uint32_t operation; /* 0 full, 1 delta, 2 remove */
  uint16_t object, generation, component;
  int64_t entity_id;
  uint64_t version;
  /* Current complete protobuf state, not the wire delta. Empty on remove.
     Pointers remain valid until the next receive/reset/destroy. */
  const uint8_t *data;
  size_t size;
} roost_view_change;
ROOST_API roost_views *roost_views_create(const uint8_t *descriptor_set,
                                          size_t size);
ROOST_API int roost_views_register(roost_views *, const char *message_type,
                                   const char *dao_namespace,
                                   const char *profile, uint32_t schema);
/* 1 success (including duplicate with zero changes), 0 failure. On failure the
   baseline is invalid: reconnect/re-subscribe for Full, never apply later
   Delta. */
ROOST_API int roost_views_receive(roost_views *, const uint8_t *, size_t,
                                  const roost_view_change **changes,
                                  size_t *count);
ROOST_API const char *roost_views_error(const roost_views *);
ROOST_API void roost_views_reset(roost_views *);
ROOST_API void roost_views_destroy(roost_views *);
#ifdef __cplusplus
}
#endif
#endif
