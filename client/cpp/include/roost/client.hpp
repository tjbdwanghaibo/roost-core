#pragma once
#include "client.h"
#include <functional>
#include <map>
#include <stdexcept>
#include <string>
#include <thread>
#include <vector>
namespace roost {
// Consumer callbacks belong to the constructing engine thread. Bytes are owned
// by each event.
struct client_event {
  uint32_t type, id, sequence, kind;
  uint64_t generation;
  std::vector<uint8_t> payload;
};
class client {
  roost_client *native_;
  const std::thread::id thread_ = std::this_thread::get_id();
  std::map<uint32_t, std::function<void(const client_event &)>> pending_;
  bool pumping_ = false;
  bool connecting_ = false;
  void check() const {
    if (std::this_thread::get_id() != thread_)
      throw std::logic_error("client consumer thread required");
  }
  void cancel_pending(const client_event &close) {
    auto callbacks = std::move(pending_);
    pending_.clear();
    for (auto &item : callbacks) {
      auto e = close;
      e.sequence = item.first;
      notify(item.second, e);
    }
  }
  void notify(const std::function<void(const client_event &)> &callback,
              const client_event &event) {
    if (!callback)
      return;
    try {
      callback(event);
    } catch (...) {
      if (on_callback_error) {
        try {
          on_callback_error();
        } catch (...) {
        }
      }
    }
  }

public:
  std::function<void(const client_event &)> on_event;
  std::function<void()> on_callback_error;
  explicit client(roost_options options = roost_default_options())
      : native_(roost_create(options)) {
    if (!native_)
      throw std::invalid_argument("invalid native client options");
  }
  ~client() { roost_destroy(native_); }
  client(const client &) = delete;
  client &operator=(const client &) = delete;
  void connect(const std::string &address, uint16_t port,
               const std::string &ticket) {
    check();
    if (pumping_ || connecting_)
      throw std::logic_error(
          "defer reconnect until the current operation returns");
    connecting_ = true;
    struct Finish {
      bool &value;
      ~Finish() { value = false; }
    } finish{connecting_};
    roost_disconnect(native_);
    cancel_pending({ROOST_CLOSED, 0, 0, 0, 0, {}});
    if (!roost_connect(native_, address.c_str(), port,
                       reinterpret_cast<const uint8_t *>(ticket.data()),
                       ticket.size()))
      throw std::runtime_error("connect admission failed");
  }
  void disconnect() {
    check();
    roost_disconnect(native_);
  }
  uint32_t request(uint32_t id, uint32_t reply,
                   const std::vector<uint8_t> &bytes,
                   std::function<void(const client_event &)> callback) {
    check();
    if (connecting_)
      throw std::logic_error("connect in progress");
    auto sequence =
        roost_request(native_, id, reply, bytes.data(), bytes.size());
    if (!sequence)
      throw std::runtime_error("request not admitted");
    pending_.emplace(sequence, std::move(callback));
    return sequence;
  }
  template <class Message>
  uint32_t request_pb(uint32_t id, uint32_t reply, const Message &message,
                      std::function<void(const client_event &)> callback) {
    std::string bytes;
    if (!message.SerializeToString(&bytes))
      throw std::runtime_error("protobuf encode failed");
    return request(id, reply, {bytes.begin(), bytes.end()},
                   std::move(callback));
  }
  void pump(size_t budget = 64) {
    check();
    if (pumping_)
      throw std::logic_error("recursive pump");
    pumping_ = true;
    struct Reset {
      bool &value;
      ~Reset() { value = false; }
    } reset{pumping_};
    roost_event raw{};
    while (budget-- && roost_poll(native_, &raw)) {
      client_event event{raw.type, raw.message_id, raw.sequence,
                         raw.kind, raw.generation, {}};
      if (raw.size)
        event.payload.assign(raw.data, raw.data + raw.size);
      if (event.type == ROOST_CLOSED)
        cancel_pending(event);
      auto found = pending_.find(event.sequence);
      if ((event.type == ROOST_RESPONSE ||
           event.type == ROOST_REQUEST_FAILED) &&
          found != pending_.end()) {
        auto callback = std::move(found->second);
        pending_.erase(found);
        notify(callback, event);
      }
      notify(on_event, event);
    }
  }
};
} // namespace roost
