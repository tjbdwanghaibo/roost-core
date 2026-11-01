#pragma once
#include <algorithm>
#include <chrono>
#include <cmath>
#include <cstdint>
#include <deque>
#include <stdexcept>
#include <utility>

namespace roost {
inline double local_seconds() {
  return std::chrono::duration<double>(
             std::chrono::steady_clock::now().time_since_epoch())
      .count();
}
struct clock_sample {
  double sent, received, server_received, server_sent;
  static clock_sample decode(double sent, double received, const uint8_t *data,
                             size_t size) {
    if (!data || size != 20 || data[0] != 'R' || data[1] != 'C' ||
        data[2] != 'N' || data[3] != '1')
      throw std::invalid_argument("invalid Gate clock response");
    auto read = [&](size_t start) {
      uint64_t n = 0;
      for (size_t i = start; i < start + 8; ++i)
        n = (n << 8) | data[i];
      if (n > uint64_t(INT64_MAX))
        throw std::invalid_argument("negative Gate clock");
      return double(n) / 1e9;
    };
    return {sent, received, read(4), read(12)};
  }
};
// 消费线程独占；换服/重连 reset。只估计样本提供者的时间域，不自动发送探针。
class network_clock {
  double offset_ = 0, target_ = 0, local_ = 0, sample_ = -INFINITY;
  double rtt_ = 0, max_rtt_, slew_;
  bool ready_ = false;

public:
  explicit network_clock(double max_rtt = 2, double slew = .05)
      : max_rtt_(max_rtt), slew_(slew) {
    if (!std::isfinite(max_rtt) || max_rtt <= 0 || !std::isfinite(slew) ||
        slew <= 0 || slew >= 1)
      throw std::invalid_argument("invalid clock limits");
  }
  bool ready() const { return ready_; }
  double round_trip() const { return rtt_; }
  double uncertainty() const {
    return ready_ ? rtt_ / 2 + std::abs(target_ - offset_) : 0;
  }
  void reset() {
    ready_ = false;
    sample_ = -INFINITY;
    rtt_ = 0;
  }
  bool observe(clock_sample s) {
    double elapsed = s.received - s.sent,
           processing = s.server_sent - s.server_received;
    double rtt = elapsed - processing;
    if (!std::isfinite(s.sent) || !std::isfinite(s.received) ||
        !std::isfinite(s.server_received) || !std::isfinite(s.server_sent) ||
        s.received <= sample_ || elapsed < 0 || processing < 0 || rtt < 0 ||
        elapsed > max_rtt_ || (ready_ && rtt > std::max(.05, rtt_ * 3)))
      return false;
    double next = s.server_sent + rtt / 2 - s.received;
    if (!ready_) {
      offset_ = next;
      local_ = s.received;
      ready_ = true;
    }
    target_ = next;
    rtt_ = rtt;
    sample_ = s.received;
    return true;
  }
  double now(double local) {
    if (!ready_)
      throw std::logic_error("clock has no sample");
    if (!std::isfinite(local) || local < local_)
      throw std::invalid_argument("clock order");
    double allowance = (local - local_) * slew_;
    offset_ += std::clamp(target_ - offset_, -allowance, allowance);
    local_ = local;
    return local + offset_;
  }
};

// 不可变表现快照；权威 EntitySync 仍立即应用。无新帧保持末值，不无界外推。
template <class T> class snapshot_buffer {
  struct sample {
    double time;
    T value;
  };
  std::deque<sample> samples_;
  size_t capacity_;
  double min_, max_, delay_, arrival_ = 0, interval_ = 0, jitter_ = 0;
  double playhead_ = -INFINITY;

public:
  explicit snapshot_buffer(size_t capacity = 32, double minimum = .05,
                           double maximum = .2)
      : capacity_(capacity), min_(minimum), max_(maximum), delay_(minimum) {
    if (capacity < 2 || capacity > 65536 || !std::isfinite(minimum) ||
        !std::isfinite(maximum) || minimum < 0 || maximum < minimum)
      throw std::invalid_argument("invalid snapshot limits");
  }
  double delay() const { return delay_; }
  size_t size() const { return samples_.size(); }
  void clear() {
    samples_.clear();
    arrival_ = interval_ = jitter_ = 0;
    delay_ = min_;
    playhead_ = -INFINITY;
  }
  bool add(double server, double arrival, T value) {
    if (!std::isfinite(server) || !std::isfinite(arrival))
      throw std::invalid_argument("snapshot time");
    if (!samples_.empty()) {
      double gap = server - samples_.back().time;
      if (gap <= 0 || arrival < arrival_)
        return false;
      jitter_ += (std::abs(arrival - arrival_ - gap) - jitter_) * .125;
      interval_ = interval_ == 0 ? gap : interval_ + (gap - interval_) * .125;
      delay_ = std::clamp(interval_ + 2 * jitter_, min_, max_);
    }
    if (samples_.size() == capacity_)
      samples_.pop_front();
    samples_.push_back({server, std::move(value)});
    arrival_ = arrival;
    return true;
  }
  template <class Interpolate>
  bool sample_at(double now, Interpolate interpolate, T &value) {
    if (!std::isfinite(now))
      throw std::invalid_argument("sample time");
    if (samples_.empty())
      return false;
    double at =
        std::max(playhead_, std::clamp(now - delay_, samples_.front().time,
                                       samples_.back().time));
    playhead_ = at;
    if (at <= samples_.front().time) {
      value = samples_.front().value;
      return true;
    }
    for (size_t i = 1; i < samples_.size(); ++i) {
      const auto &a = samples_[i - 1], &b = samples_[i];
      if (at <= b.time) {
        value =
            interpolate(a.value, b.value, (at - a.time) / (b.time - a.time));
        return true;
      }
    }
    value = samples_.back().value;
    return true;
  }
};
} // namespace roost
