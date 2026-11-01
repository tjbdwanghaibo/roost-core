#include "roost/client.h"
#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstring>
#include <deque>
#include <map>
#include <mutex>
#include <set>
#include <stdexcept>
#include <string>
#include <thread>
#include <vector>
#ifdef _WIN32
#include <winsock2.h>
#include <ws2tcpip.h>
using Socket = SOCKET;
static constexpr Socket invalid_socket = INVALID_SOCKET;
static void close_socket(Socket s) { closesocket(s); }
static bool blocked() {
  int e = WSAGetLastError();
  return e == WSAEWOULDBLOCK || e == WSAEINPROGRESS;
}
#else
#include <cerrno>
#include <fcntl.h>
#include <netdb.h>
#include <sys/select.h>
#include <sys/socket.h>
#include <unistd.h>
using Socket = int;
static constexpr Socket invalid_socket = -1;
static void close_socket(Socket s) { close(s); }
static bool blocked() {
  return errno == EWOULDBLOCK || errno == EAGAIN || errno == EINPROGRESS ||
         errno == EINTR;
}
#endif
using Bytes = std::vector<uint8_t>;
using Clock = std::chrono::steady_clock;
static uint32_t u32(const uint8_t *p) {
  return uint32_t(p[0]) << 24 | uint32_t(p[1]) << 16 | uint32_t(p[2]) << 8 |
         p[3];
}
static void put32(Bytes &b, size_t at, uint32_t v) {
  for (int i = 3; i >= 0; --i) {
    b[at + i] = uint8_t(v);
    v >>= 8;
  }
}
static Bytes packet(uint32_t id, uint32_t seq, const Bytes &body,
                    uint32_t kind = 0) {
  Bytes out(16 + body.size());
  out[0] = 'R';
  out[1] = 'S';
  out[2] = 2;
  out[3] = uint8_t(kind << 1);
  put32(out, 4, id);
  put32(out, 8, seq);
  put32(out, 12, uint32_t(body.size()));
  std::copy(body.begin(), body.end(), out.begin() + 16);
  return out;
}
struct Event {
  uint32_t type = 0, id = 0, seq = 0, kind = 0;
  uint64_t generation = 0;
  Bytes body;
  uint32_t status = ROOST_OK;
};
struct Request {
  uint32_t id, reply, seq, kind;
  Bytes body;
  Clock::time_point deadline;
};
struct roost_client {
  roost_options options;
  std::mutex mutex;
  std::deque<Request> commands;
  std::deque<Event> events;
  Event current;
  std::thread worker;
  std::atomic<bool> stop{false}, ready{false};
  std::atomic<uint32_t> sequence{1};
  std::set<uint32_t> active, canceled;
  uint64_t generation = 0;
  explicit roost_client(roost_options o) : options(o) {}
  // 网络线程独占 socket，包括关闭；主线程只投递命令和取事件，不能同时 close。
  // One worker owns every socket operation, including close. Shutdown is
  // bounded by select's 10ms poll.
  bool emit(Event e) {
    std::lock_guard<std::mutex> lock(mutex);
    if (events.size() >= options.max_events)
      return false;
    e.generation = generation;
    events.push_back(std::move(e));
    return true;
  }
  void fail(const std::string &reason) {
    ready = false;
    std::lock_guard<std::mutex> lock(mutex);
    // A terminal close invalidates every outstanding request and the entire
    // Sync baseline.
    commands.clear();
    events.clear();
    active.clear();
    canceled.clear();
    events.push_back(Event{ROOST_CLOSED, 0, 0, 0, generation,
                           Bytes(reason.begin(), reason.end())});
  }
  bool take_cancellation(uint32_t seq) {
    std::lock_guard<std::mutex> lock(mutex);
    return canceled.erase(seq) != 0;
  }
  void run(std::string address, uint16_t port, Bytes ticket) {
    Socket socket = invalid_socket;
    try {
#ifdef _WIN32
      WSADATA startup{};
      if (WSAStartup(MAKEWORD(2, 2), &startup) != 0)
        throw std::runtime_error("WSAStartup failed");
      struct Cleanup {
        Socket &socket;
        ~Cleanup() {
          if (socket != invalid_socket) {
            close_socket(socket);
            socket = invalid_socket;
          }
          WSACleanup();
        }
      } cleanup{socket};
#endif
      addrinfo hints{};
      hints.ai_socktype = SOCK_STREAM;
      hints.ai_flags = AI_NUMERICHOST | AI_NUMERICSERV;
      addrinfo *list = nullptr;
      auto service = std::to_string(port);
      if (getaddrinfo(address.c_str(), service.c_str(), &hints, &list) != 0)
        throw std::runtime_error("numeric endpoint required");
      struct Addresses {
        addrinfo *value;
        ~Addresses() { freeaddrinfo(value); }
      } addresses{list};
      socket = ::socket(list->ai_family, SOCK_STREAM, 0);
      if (socket == invalid_socket)
        throw std::runtime_error("socket failed");
#ifdef _WIN32
      u_long one = 1;
      if (ioctlsocket(socket, FIONBIO, &one) != 0)
        throw std::runtime_error("nonblocking failed");
#else
      if (socket >= FD_SETSIZE)
        throw std::runtime_error("socket exceeds select limit");
      if (fcntl(socket, F_SETFL, fcntl(socket, F_GETFL, 0) | O_NONBLOCK) < 0)
        throw std::runtime_error("nonblocking failed");
#ifdef SO_NOSIGPIPE
      int one = 1;
      setsockopt(socket, SOL_SOCKET, SO_NOSIGPIPE, &one, sizeof(one));
#endif
#endif
      bool connected =
          ::connect(socket, list->ai_addr, int(list->ai_addrlen)) == 0;
      if (!connected && !blocked())
        throw std::runtime_error("connect failed");
      const auto connect_deadline =
          Clock::now() + std::chrono::milliseconds(options.connect_timeout_ms);
      Bytes incoming, outgoing = packet(0, 1, ticket);
      auto outgoing_deadline = connect_deadline;
      size_t sent = 0;
      uint32_t notify_seq = 0, notify_id = 0, notify_kind = 0;
      std::map<uint32_t, Request> pending;
      auto heartbeat_at = Clock::now();
      uint32_t heartbeat_seq = 0;
      Clock::time_point heartbeat_deadline;
      while (!stop) {
        auto now = Clock::now();
        if (!ready && now >= connect_deadline)
          throw std::runtime_error("connect/authentication timeout");
        if (!outgoing.empty() && now >= outgoing_deadline)
          throw std::runtime_error("socket send timeout: result unknown");
        if (heartbeat_seq && now >= heartbeat_deadline)
          throw std::runtime_error("heartbeat timeout");
        for (auto it = pending.begin(); it != pending.end();) {
          bool canceled = take_cancellation(it->first);
          if (!canceled && now < it->second.deadline) {
            ++it;
            continue;
          }
          std::string reason =
              canceled ? "request canceled after send: result unknown"
                       : "request timeout: result unknown";
          if (!emit(Event{ROOST_REQUEST_FAILED, it->second.id, it->first, 0, 0,
                          Bytes(reason.begin(), reason.end()),
                          canceled ? ROOST_CANCELED_UNKNOWN
                                   : ROOST_TIMEOUT_UNKNOWN}))
            throw std::runtime_error("event queue overflow");
          it = pending.erase(it);
        }
        if (ready && outgoing.empty()) {
          Request request{};
          bool found = false;
          bool heartbeat = false;
          {
            std::lock_guard<std::mutex> lock(mutex);
            if (!commands.empty()) {
              request = std::move(commands.front());
              commands.pop_front();
              found = true;
            } else if (options.heartbeat_ms && !heartbeat_seq &&
                       now >= heartbeat_at) {
              // 选包与心跳取号必须和 admit 同锁，确保线上的序号沿发送顺序递增。
              heartbeat_seq = sequence.fetch_add(1) + 1;
              if (heartbeat_seq == 0)
                throw std::runtime_error("sequence exhausted");
              heartbeat = true;
            }
          }
          if (found) {
            bool canceled = take_cancellation(request.seq);
            if (canceled || now >= request.deadline) {
              const std::string reason = canceled
                                             ? "request canceled before send"
                                             : "request expired before send";
              if (!emit(Event{ROOST_REQUEST_FAILED, request.id, request.seq, 0,
                              0, Bytes(reason.begin(), reason.end()),
                              canceled ? ROOST_CANCELED_UNSENT
                                       : ROOST_TIMEOUT_UNSENT}))
                throw std::runtime_error("event queue overflow");
            } else {
              outgoing =
                  packet(request.id, request.seq, request.body, request.kind);
              outgoing_deadline = request.deadline;
              sent = 0;
              if (request.reply || request.id == 0)
                pending.emplace(request.seq, std::move(request));
              else {
                notify_seq = request.seq;
                notify_id = request.id;
                notify_kind = request.kind;
              }
            }
          } else if (heartbeat) {
            outgoing = packet(0, heartbeat_seq, {});
            sent = 0;
            heartbeat_deadline = now + std::chrono::milliseconds(
                                           options.heartbeat_timeout_ms
                                               ? options.heartbeat_timeout_ms
                                               : options.request_timeout_ms);
            outgoing_deadline = heartbeat_deadline;
          }
        }
        fd_set reads, writes;
        FD_ZERO(&reads);
        FD_ZERO(&writes);
        FD_SET(socket, &reads);
        if (!connected || !outgoing.empty())
          FD_SET(socket, &writes);
        timeval wait{0, 10000};
        int result = select(int(socket) + 1, &reads, &writes, nullptr, &wait);
        if (result < 0) {
          if (blocked())
            continue;
          throw std::runtime_error("select failed");
        }
        if (!connected && FD_ISSET(socket, &writes)) {
          int error = 0;
#ifdef _WIN32
          int length = sizeof(error);
#else
          socklen_t length = sizeof(error);
#endif
          if (getsockopt(socket, SOL_SOCKET, SO_ERROR,
                         reinterpret_cast<char *>(&error), &length) != 0 ||
              error)
            throw std::runtime_error("connect failed");
          connected = true;
        }
        if (connected && !outgoing.empty() && FD_ISSET(socket, &writes)) {
          int flags = 0;
#ifdef MSG_NOSIGNAL
          flags = MSG_NOSIGNAL;
#endif
          int count = int(send(
              socket, reinterpret_cast<const char *>(outgoing.data() + sent),
              int(outgoing.size() - sent), flags));
          if (count < 0 && !blocked())
            throw std::runtime_error("send failed");
          if (count > 0) {
            sent += size_t(count);
            if (sent == outgoing.size()) {
              outgoing.clear();
              sent = 0;
              if (notify_seq) {
                if (!emit(Event{
                        ROOST_SENT, notify_id, notify_seq, notify_kind, 0, {}}))
                  throw std::runtime_error("event queue overflow");
                notify_seq = 0;
              }
            }
          }
        }
        if (!connected || !FD_ISSET(socket, &reads))
          continue;
        uint8_t buffer[16384];
        int count = int(
            recv(socket, reinterpret_cast<char *>(buffer), sizeof(buffer), 0));
        if (count == 0)
          throw std::runtime_error("peer closed");
        if (count < 0) {
          if (blocked())
            continue;
          throw std::runtime_error("receive failed");
        }
        incoming.insert(incoming.end(), buffer, buffer + count);
        size_t offset = 0;
        while (incoming.size() - offset >= 16) {
          const auto *head = incoming.data() + offset;
          uint32_t length = u32(head + 12), id = u32(head + 4),
                   seq = u32(head + 8);
          uint8_t flags = head[3];
          if (head[0] != 'R' || head[1] != 'S' || head[2] != 2 ||
              (flags & ~7) || (flags & 6) == 6 || (!id && flags) ||
              length > options.max_payload)
            throw std::runtime_error("invalid RS frame");
          if (incoming.size() - offset < 16 + size_t(length))
            break;
          Bytes body(head + 16, head + 16 + length);
          offset += 16 + length;
          if (!ready) {
            if (id || seq != 1 || !body.empty())
              throw std::runtime_error("authentication refused");
            ready = true;
            heartbeat_at =
                now + std::chrono::milliseconds(options.heartbeat_ms);
            if (!emit(Event{ROOST_READY, 0, 0, 0, 0, {}}))
              throw std::runtime_error("event queue overflow");
            continue;
          }
          if (!id && seq == heartbeat_seq) {
            heartbeat_seq = 0;
            heartbeat_at =
                now + std::chrono::milliseconds(options.heartbeat_ms);
            continue;
          }
          if (flags & 1) {
            if (!emit(Event{ROOST_PUSH, id, seq, uint32_t((flags >> 1) & 3), 0,
                            std::move(body)}))
              throw std::runtime_error("event queue overflow");
            continue;
          }
          auto found = pending.find(seq);
          if (found == pending.end())
            continue;
          if (id != found->second.reply || (flags >> 1) != 0)
            throw std::runtime_error("unexpected response route");
          pending.erase(found);
          if (!emit(Event{ROOST_RESPONSE, id, seq, 0, 0, std::move(body)}))
            throw std::runtime_error("event queue overflow");
        }
        incoming.erase(incoming.begin(), incoming.begin() + offset);
      }
      fail("disconnected");
    } catch (const std::exception &e) {
      fail(e.what());
    } catch (...) {
      fail("native client failed");
    }
    if (socket != invalid_socket)
      close_socket(socket);
  }
};
extern "C" {
uint32_t roost_abi_version() { return 2; }
roost_options roost_default_options() {
  return {1u << 20, 256, 128, 5000, 5000, 15000, 5000};
}
roost_client *roost_create(roost_options o) {
  try {
    if (!o.max_payload || o.max_payload > (16u << 20) || !o.max_events ||
        !o.max_requests || !o.connect_timeout_ms || !o.request_timeout_ms)
      return nullptr;
    return new roost_client(o);
  } catch (...) {
    return nullptr;
  }
}
void roost_disconnect(roost_client *c) {
  if (!c)
    return;
  c->stop = true;
  if (c->worker.joinable())
    c->worker.join();
  c->ready = false;
}
void roost_destroy(roost_client *c) {
  if (!c)
    return;
  roost_disconnect(c);
  delete c;
}
int roost_connect(roost_client *c, const char *address, uint16_t port,
                  const uint8_t *ticket, size_t size) {
  if (!c || !address || !port || (!ticket && size) ||
      size > c->options.max_payload)
    return 0;
  try {
    roost_disconnect(c);
    {
      std::lock_guard<std::mutex> lock(c->mutex);
      c->events.clear();
      c->commands.clear();
      c->active.clear();
      c->canceled.clear();
    }
    c->stop = false;
    c->sequence = 1;
    ++c->generation;
    Bytes bytes;
    if (size)
      bytes.assign(ticket, ticket + size);
    c->worker = std::thread([c, address = std::string(address), port,
                             bytes = std::move(bytes)]() mutable {
      c->run(address, port, std::move(bytes));
    });
    return 1;
  } catch (...) {
    return 0;
  }
}
static uint32_t admit(roost_client *c, uint32_t id, uint32_t reply,
                      uint32_t kind, const uint8_t *data, size_t size,
                      uint32_t timeout_ms = 0, bool control = false) {
  if (!c || (!id && !control) || (kind != 0 && kind != 2) || (!data && size) ||
      size > c->options.max_payload)
    return 0;
  try {
    std::lock_guard<std::mutex> lock(c->mutex);
    if (!c->ready || c->stop || c->active.size() >= c->options.max_requests)
      return 0;
    uint32_t seq = c->sequence.fetch_add(1) + 1;
    if (!seq) {
      c->stop = true;
      return 0;
    }
    Bytes body;
    if (size)
      body.assign(data, data + size);
    c->active.insert(seq);
    try {
      c->commands.push_back(Request{
          id, reply, seq, kind, std::move(body),
          Clock::now() +
              std::chrono::milliseconds(
                  timeout_ms ? timeout_ms : c->options.request_timeout_ms)});
    } catch (...) {
      c->active.erase(seq);
      throw;
    }
    return seq;
  } catch (...) {
    return 0;
  }
}
uint32_t roost_request(roost_client *c, uint32_t id, uint32_t reply,
                       const uint8_t *data, size_t size) {
  return reply ? admit(c, id, reply, 0, data, size) : 0;
}
uint32_t roost_request_with_timeout(roost_client *c, uint32_t id,
                                    uint32_t reply, const uint8_t *data,
                                    size_t size, uint32_t timeout_ms) {
  return reply ? admit(c, id, reply, 0, data, size, timeout_ms) : 0;
}
uint32_t roost_notify(roost_client *c, uint32_t id, uint32_t kind,
                      const uint8_t *data, size_t size) {
  return admit(c, id, 0, kind, data, size);
}
uint32_t roost_clock_probe(roost_client *c, uint32_t timeout_ms) {
  const uint8_t payload[] = {'R', 'C', 'N', '1'};
  return admit(c, 0, 0, 0, payload, sizeof(payload), timeout_ms, true);
}
int roost_cancel(roost_client *c, uint32_t seq) {
  if (!c || !seq)
    return 0;
  try {
    std::lock_guard<std::mutex> lock(c->mutex);
    if (!c->active.count(seq))
      return 0;
    c->canceled.insert(seq);
    return 1;
  } catch (...) {
    return 0;
  }
}
int roost_poll(roost_client *c, roost_event *out) {
  if (!c || !out)
    return 0;
  try {
    std::lock_guard<std::mutex> lock(c->mutex);
    if (c->events.empty())
      return 0;
    c->current = std::move(c->events.front());
    c->events.pop_front();
    auto &e = c->current;
    if (e.type == ROOST_RESPONSE || e.type == ROOST_REQUEST_FAILED ||
        e.type == ROOST_SENT) {
      c->active.erase(e.seq);
      c->canceled.erase(e.seq);
    }
    *out = {e.type,       e.id,          e.seq,         e.kind,
            e.generation, e.body.data(), e.body.size(), e.status};
    return 1;
  } catch (...) {
    return 0;
  }
}
}
