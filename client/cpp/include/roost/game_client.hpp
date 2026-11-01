#pragma once
#include "client.hpp"
#include "entity_views.hpp"
#include "lockstep.hpp"
#include <chrono>
#include <optional>
namespace roost {
struct connection_ticket {
  std::string address, ticket;
  uint16_t port;
};
struct reconnect_options {
  bool enabled = false;
  size_t max_attempts = 8;
  std::chrono::milliseconds initial_delay{1000}, max_delay{30000};
};
// 游戏消费线程入口。provider 只返回异步账号/DNS流程已经准备好的地址与新票据，
// nullopt 表示尚未准备好，不阻塞 Pump。重连不重放业务请求，Lockstep
// 必须重新绑定。
class game_client {
  client connection_;
  const std::thread::id thread_ = std::this_thread::get_id();
  bool connecting_ = false;
  void check() const {
    if (std::this_thread::get_id() != thread_)
      throw std::logic_error("game client consumer thread required");
  }
  std::map<uint32_t, std::function<void(const client_event &)>> handlers_;
  std::map<uint32_t, std::function<void(const bytes &)>> locksteps_;
  reconnect_options retry_;
  std::chrono::steady_clock::time_point retry_at_{};
  std::chrono::milliseconds delay_{};
  size_t attempts_ = 0;
  bool was_ready_ = false, recovering_ = false, waiting_ = false,
       stopped_ = false, pumping_ = false;
  void error() noexcept {
    if (on_error)
      try {
        on_error(std::current_exception());
      } catch (...) {
      }
  }
  template <class F> void callback(F work) {
    try {
      work();
    } catch (...) {
      error();
    }
  }
  void reset() {
    bool had = !entities.current().empty();
    entities.reset();
    locksteps_.clear();
    if (had && on_entities_reset)
      callback([&] { on_entities_reset(); });
  }
  void schedule() {
    if (stopped_ || !retry_.enabled || !was_ready_)
      return;
    if (attempts_ >= retry_.max_attempts) {
      recovering_ = false;
      if (on_reconnect_exhausted)
        callback([&] { on_reconnect_exhausted(); });
      return;
    }
    waiting_ = true;
    recovering_ = true;
    retry_at_ = std::chrono::steady_clock::now() + delay_;
    delay_ =
        delay_ >= retry_.max_delay - delay_ ? retry_.max_delay : delay_ * 2;
  }
  void event(const client_event &e) {
    if (e.type == ROOST_READY) {
      bool recovered = recovering_;
      was_ready_ = true;
      recovering_ = waiting_ = false;
      attempts_ = 0;
      delay_ = retry_.initial_delay;
      if (on_ready)
        callback([&] { on_ready(); });
      if (recovered && on_reconnected)
        callback([&] { on_reconnected(); });
    } else if (e.type == ROOST_CLOSED) {
      reset();
      if (on_closed)
        callback([&] { on_closed(e); });
      schedule();
    } else if (e.type == ROOST_PUSH) {
      if (e.kind == 1) {
        std::vector<protobuf_change> changes;
        try {
          entities.receive(e.payload.data(), e.payload.size(), changes);
        } catch (...) {
          connection_.disconnect();
          reset();
          error();
          return;
        }
        for (const auto &change : changes)
          if (on_entity_changed)
            callback([&] { on_entity_changed(change); });
      } else {
        auto simulation = locksteps_.find(e.id);
        if (e.kind == 2 && simulation != locksteps_.end()) {
          auto run = simulation->second;
          callback([&] { run(e.payload); });
        } else {
          auto found = handlers_.find(e.id);
          if (e.kind == 0 && found != handlers_.end()) {
            auto run = found->second;
            callback([&] { run(e); });
          } else if (on_unhandled_push)
            callback([&] { on_unhandled_push(e); });
        }
      }
    }
  }

public:
  entity_views entities;
  std::function<std::optional<connection_ticket>(size_t)> reconnect_ticket;
  std::function<void()> on_ready, on_reconnected, on_entities_reset,
      on_reconnect_exhausted;
  std::function<void(size_t)> on_reconnect_attempt;
  std::function<void(const client_event &)> on_closed, on_unhandled_push;
  std::function<void(const protobuf_change &)> on_entity_changed;
  std::function<void(std::exception_ptr)> on_error;
  explicit game_client(roost_options options = default_options())
      : connection_(options) {
    connection_.on_event = [this](const client_event &e) { event(e); };
    connection_.on_callback_error = [this] { error(); };
  }
  game_client(const game_client &) = delete;
  game_client &operator=(const game_client &) = delete;
  client &transport() {
    check();
    return connection_;
  }
  void configure_reconnect(reconnect_options options) {
    check();
    if (!options.max_attempts || options.initial_delay.count() <= 0 ||
        options.max_delay < options.initial_delay)
      throw std::invalid_argument("invalid reconnect policy");
    retry_ = options;
    delay_ = options.initial_delay;
    if (!options.enabled)
      waiting_ = recovering_ = false;
  }
  void connect(const connection_ticket &ticket) {
    check();
    if (pumping_ || connecting_)
      throw std::logic_error("connect after current operation returns");
    connecting_ = true;
    struct finish {
      bool &v;
      ~finish() { v = false; }
    } guard{connecting_};
    stopped_ = false;
    was_ready_ = recovering_ = waiting_ = false;
    attempts_ = 0;
    delay_ = retry_.initial_delay;
    reset();
    connection_.connect(ticket.address, ticket.port, ticket.ticket);
  }
  void disconnect() {
    check();
    stopped_ = true;
    waiting_ = recovering_ = false;
    connection_.disconnect();
  }
  // 替换/删除 ID 路由在消费线程进行；复制回调后调用，允许回调自行注销。
  template <class Message>
  void subscribe(uint32_t id, std::function<void(const Message &)> handler) {
    check();
    if (!id || !handler)
      throw std::invalid_argument("push ID/handler required");
    handlers_[id] = [handler = std::move(handler)](const client_event &e) {
      Message message;
      if (!message.ParseFromArray(e.payload.data(), int(e.payload.size())))
        throw std::runtime_error("invalid push protobuf");
      handler(message);
    };
  }
  void unsubscribe(uint32_t id) {
    check();
    handlers_.erase(id);
  }
  void bind_lockstep(uint32_t broadcast_id,
                     std::function<void(const bytes &)> receive) {
    check();
    if (!broadcast_id || !receive)
      throw std::invalid_argument("Lockstep ID/receiver required");
    locksteps_[broadcast_id] = std::move(receive);
  }
  uint32_t probe_clock(std::function<void(clock_sample)> completed,
                       std::function<void(const client_event &)> failed,
                       uint32_t timeout_ms = 0) {
    check();
    if (!completed || !failed)
      throw std::invalid_argument("clock callbacks required");
    double sent = local_seconds();
    return connection_.clock_probe(
        [sent, completed, failed](const client_event &e) {
          if (e.type != ROOST_RESPONSE) {
            failed(e);
            return;
          }
          clock_sample sample;
          try {
            sample = clock_sample::decode(sent, local_seconds(),
                                          e.payload.data(), e.payload.size());
          } catch (...) {
            auto invalid = e;
            invalid.type = ROOST_REQUEST_FAILED;
            invalid.status = ROOST_INVALID_PAYLOAD;
            failed(invalid);
            return;
          }
          completed(sample);
        },
        timeout_ms);
  }
  template <class Response, class Request>
  uint32_t request(uint32_t id, uint32_t reply, const Request &request,
                   std::function<void(const Response &)> completed,
                   std::function<void(const client_event &)> failed,
                   uint32_t timeout_ms = 0) {
    check();
    if (!completed || !failed)
      throw std::invalid_argument("request callbacks required");
    return connection_.request_pb(
        id, reply, request,
        [completed = std::move(completed),
         failed = std::move(failed)](const client_event &e) {
          if (e.type != ROOST_RESPONSE) {
            failed(e);
            return;
          }
          Response response;
          if (!response.ParseFromArray(e.payload.data(),
                                       int(e.payload.size()))) {
            auto malformed = e;
            malformed.type = ROOST_REQUEST_FAILED;
            malformed.status = ROOST_INVALID_PAYLOAD;
            failed(malformed);
            return;
          }
          completed(response);
        },
        timeout_ms);
  }
  void pump(size_t budget = 64) {
    check();
    if (pumping_ || connecting_)
      throw std::logic_error("recursive Pump");
    pumping_ = true;
    struct finish {
      bool &v;
      ~finish() { v = false; }
    } guard{pumping_};
    connection_.pump(budget);
    if (!waiting_ || std::chrono::steady_clock::now() < retry_at_ ||
        !reconnect_ticket)
      return;
    bool attempted = false;
    try {
      auto ticket = reconnect_ticket(attempts_ + 1);
      if (!ticket || stopped_ || !waiting_)
        return;
      waiting_ = false;
      ++attempts_;
      attempted = true;
      if (on_reconnect_attempt)
        callback([&] { on_reconnect_attempt(attempts_); });
      if (stopped_)
        return;
      connection_.connect(ticket->address, ticket->port, ticket->ticket);
    } catch (...) {
      if (!attempted)
        ++attempts_;
      error();
      waiting_ = false;
      schedule();
    }
  }
};
} // namespace roost
