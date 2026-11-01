#pragma once
#include "network_time.hpp"
#include <vector>
namespace roost {
enum class client_stage { apply, present };
struct client_trace_event {
  client_stage stage;
  uint32_t epoch, tick;
  double local_time, duration = 0;
};
// 消费线程有界环；不保留 payload。Epoch/Tick
// 与服务端日志关联，不能直接跨机减时间戳。
class client_trace {
  std::vector<client_trace_event> ring_;
  size_t head_ = 0, count_ = 0;
  uint64_t overwritten_ = 0;

public:
  explicit client_trace(size_t capacity = 1024) {
    if (!capacity || capacity > 1048576)
      throw std::invalid_argument("trace capacity");
    ring_.resize(capacity);
  }
  uint64_t overwritten() const { return overwritten_; }
  void record(client_trace_event event) {
    if (count_ == ring_.size()) {
      head_ = (head_ + 1) % ring_.size();
      --count_;
      ++overwritten_;
    }
    ring_[(head_ + count_++) % ring_.size()] = event;
  }
  std::vector<client_trace_event> drain() {
    std::vector<client_trace_event> result;
    result.reserve(count_);
    for (size_t i = 0; i < count_; ++i)
      result.push_back(ring_[(head_ + i) % ring_.size()]);
    head_ = count_ = 0;
    return result;
  }
};
} // namespace roost
