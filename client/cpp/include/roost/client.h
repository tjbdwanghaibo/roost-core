#ifndef ROOST_CLIENT_H
#define ROOST_CLIENT_H
#include <stddef.h>
#include <stdint.h>
#if defined(_WIN32) && defined(ROOST_SHARED)
#if defined(ROOST_BUILD)
#define ROOST_API __declspec(dllexport)
#else
#define ROOST_API __declspec(dllimport)
#endif
#elif defined(__GNUC__)
#define ROOST_API __attribute__((visibility("default")))
#else
#define ROOST_API
#endif
#ifdef __cplusplus
extern "C" {
#endif
typedef struct roost_client roost_client;
/* C ABI does not expose C++ exceptions, allocators, STL or engine objects. */
typedef struct roost_options {
  uint32_t max_payload, max_events, max_requests;
  uint32_t connect_timeout_ms, request_timeout_ms, heartbeat_ms;
} roost_options;
enum roost_event_type {
  ROOST_READY = 1,
  ROOST_RESPONSE = 2,
  ROOST_PUSH = 3,
  ROOST_REQUEST_FAILED = 4,
  ROOST_CLOSED = 5
};
typedef struct roost_event {
  uint32_t type, message_id, sequence, kind;
  uint64_t generation;
  /* Valid until next poll or destroy. Copy before crossing threads. */
  const uint8_t *data;
  size_t size;
} roost_event;
ROOST_API roost_options roost_default_options(void);
ROOST_API roost_client *roost_create(roost_options options);
/* Numeric IPv4/IPv6 endpoint. Resolve DNS via the engine/platform before
   connect. Lifecycle calls and poll belong to one engine thread. Requests may
   be concurrent. Reconnect is explicit, with a fresh ticket; requests are never
   replayed. */
ROOST_API int roost_connect(roost_client *, const char *address, uint16_t port,
                            const uint8_t *ticket, size_t size);
ROOST_API uint32_t roost_request(roost_client *, uint32_t message_id,
                                 uint32_t response_id, const uint8_t *data,
                                 size_t size);
ROOST_API int roost_poll(roost_client *, roost_event *event);
ROOST_API void roost_disconnect(roost_client *);
ROOST_API void roost_destroy(roost_client *);
#ifdef __cplusplus
}
#endif
#endif
