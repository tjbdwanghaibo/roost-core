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
  uint32_t heartbeat_timeout_ms; /* 0 uses request_timeout_ms */
} roost_options;
enum roost_event_type {
  ROOST_READY = 1,
  ROOST_RESPONSE = 2,
  ROOST_PUSH = 3,
  ROOST_REQUEST_FAILED = 4,
  ROOST_CLOSED = 5,
  ROOST_SENT = 6
};
enum roost_request_status {
  ROOST_OK = 0,
  ROOST_TIMEOUT_UNSENT = 1,
  ROOST_TIMEOUT_UNKNOWN = 2,
  ROOST_CANCELED_UNSENT = 3,
  ROOST_CANCELED_UNKNOWN = 4,
  ROOST_INVALID_PAYLOAD = 5
};
typedef struct roost_event {
  uint32_t type, message_id, sequence, kind;
  uint64_t generation;
  /* Valid until next poll or destroy. Copy before crossing threads. */
  const uint8_t *data;
  size_t size;
  uint32_t status;
} roost_event;
/* ABI 2 adds request status and heartbeat_timeout_ms. Rebuild all engine
 * bindings together.
 */
ROOST_API uint32_t roost_abi_version(void);
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
/* Per-request timeout starts at admission; 0 uses the configured default. */
ROOST_API uint32_t roost_request_with_timeout(roost_client *,
                                              uint32_t message_id,
                                              uint32_t response_id,
                                              const uint8_t *data, size_t size,
                                              uint32_t timeout_ms);
/* Notify uses PB(0) or Lockstep(2), and completes with SENT once written to
   the socket (not a server acknowledgement). Both paths share bounded capacity.
 */
ROOST_API uint32_t roost_notify(roost_client *, uint32_t message_id,
                                uint32_t kind, const uint8_t *data,
                                size_t size);
/* Best-effort cancellation: response and cancellation may race. A sent request
   remains an unknown business result; the library never retries it. */
ROOST_API int roost_cancel(roost_client *, uint32_t sequence);
ROOST_API int roost_poll(roost_client *, roost_event *event);
ROOST_API void roost_disconnect(roost_client *);
ROOST_API void roost_destroy(roost_client *);
#ifdef __cplusplus
}
#endif
#endif
