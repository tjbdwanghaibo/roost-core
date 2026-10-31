#include "roost/lockstep.h"
#include "roost/lockstep.hpp"
#include <array>
#include <cstdio>
namespace {
roost::bytes copy_bytes(const uint8_t *data, size_t size) {
  if (!data && size)
    throw std::invalid_argument("null payload");
  return size ? roost::bytes(data, data + size) : roost::bytes{};
}
} // namespace
struct roost_lockstep {
  roost_lockstep_callbacks callbacks;
  std::array<char, 256> error{};
  bool calling = false;
  roost::lockstep_client client;
  roost_lockstep(roost_lockstep_callbacks cb, size_t buffered, size_t retry,
                 size_t snapshot)
      : callbacks(cb),
        client(
            [this](const roost::bytes &data) {
              if (!callbacks.send(callbacks.context, data.data(), data.size()))
                throw std::runtime_error("Lockstep send not admitted");
            },
            cb.restore
                ? roost::lockstep_client::restore(
                      [this](const roost::lockstep_snapshot &s) {
                        uint64_t hash = 0;
                        if (!callbacks.restore(callbacks.context, s.frame,
                                               s.data.data(), s.data.size(),
                                               s.state_hash, s.has_state_hash,
                                               &hash))
                          throw std::runtime_error("Lockstep restore failed");
                        return hash;
                      })
                : roost::lockstep_client::restore{},
            buffered, retry, snapshot) {}
  template <class F> int call(F work) noexcept {
    if (calling)
      return 0;
    calling = true;
    try {
      work();
      error[0] = 0;
      calling = false;
      return 1;
    } catch (const std::exception &e) {
      std::snprintf(error.data(), error.size(), "%s", e.what());
    } catch (...) {
      std::snprintf(error.data(), error.size(), "%s",
                    "unknown Lockstep failure");
    }
    calling = false;
    return 0;
  }
};
extern "C" {
roost_lockstep *roost_lockstep_create(roost_lockstep_callbacks cb,
                                      size_t buffered, size_t retry,
                                      size_t snapshot) {
  if (!cb.send || !cb.simulate)
    return nullptr;
  try {
    return new roost_lockstep(cb, buffered, retry, snapshot);
  } catch (...) {
    return nullptr;
  }
}
int roost_lockstep_receive(roost_lockstep *s, const uint8_t *data,
                           size_t size) {
  if (!s)
    return 0;
  return s->call([&] {
    if (size > (64u << 20))
      throw std::invalid_argument("Lockstep packet exceeds limit");
    s->client.receive(
        copy_bytes(data, size), [&](const roost::lockstep_frame &frame) {
          std::vector<roost_lockstep_input> inputs;
          for (const auto &input : frame.inputs)
            inputs.push_back(
                {input.player, input.payload.data(), input.payload.size()});
          if (!s->callbacks.simulate(s->callbacks.context, frame.id,
                                     inputs.data(), inputs.size()))
            throw std::runtime_error("Lockstep simulation failed");
        });
  });
}
int roost_lockstep_submit(roost_lockstep *s, uint32_t frame,
                          const uint8_t *data, size_t size) {
  if (!s)
    return 0;
  return s->call([&] {
    if (size > 1024)
      throw std::invalid_argument("Lockstep input exceeds limit");
    s->client.submit_input(frame, copy_bytes(data, size));
  });
}
int roost_lockstep_hash(roost_lockstep *s, uint32_t frame, uint64_t hash) {
  if (!s)
    return 0;
  return s->call([&] { s->client.report_hash(frame, hash); });
}
int roost_lockstep_catchup(roost_lockstep *s, uint32_t frame) {
  if (!s)
    return 0;
  return s->call([&] { s->client.request_catchup(frame); });
}
uint64_t roost_lockstep_next(const roost_lockstep *s) {
  return s ? s->client.next() : 0;
}
const char *roost_lockstep_error(const roost_lockstep *s) {
  return s ? s->error.data() : "null Lockstep client";
}
void roost_lockstep_destroy(roost_lockstep *s) {
  if (s && !s->calling)
    delete s;
}
}
