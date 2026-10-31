#pragma once
#include <cmath>
#include <cstdint>
#include <limits>
#include <stdexcept>
namespace roost {
// 场景时间域：输入为估计的服务端业务纳秒，不使用客户端墙钟替代业务时间。
// 参数来自场景同步；Now 单调，跨场景应创建新实例。算术超范围明确拒绝，避免 C++
// 有符号溢出。
class scene_time_domain {
  int64_t business_ = 0, domain_ = 0, high_ = INT64_MIN;
  double rate_ = 1;

public:
  void update(int64_t business, int64_t domain, double rate) {
    if (!std::isfinite(rate) || rate < 0)
      throw std::invalid_argument("time rate must be finite and non-negative");
    business_ = business;
    domain_ = domain;
    rate_ = rate;
  }
  double rate() const { return business_ == 0 ? 1 : rate_; }
  int64_t at(int64_t business) const {
    if (!business_)
      return business;
    if (business <= business_ || rate_ == 0)
      return domain_;
    // unsigned subtraction preserves the exact positive difference even across
    // zero.
    uint64_t delta = uint64_t(business) - uint64_t(business_);
    if (rate_ == 1) {
      uint64_t room = uint64_t(INT64_MAX) - uint64_t(domain_);
      if (delta > room)
        throw std::overflow_error("time domain overflow");
      if (delta <= uint64_t(INT64_MAX)) {
        auto step = int64_t(delta);
        return domain_ + step;
      }
      // delta > INT64_MAX implies a negative domain anchor; result is
      // non-negative.
      return int64_t(delta - (uint64_t(-(domain_ + 1)) + 1));
    }
    // Same double multiplication/floor as the server and managed client for
    // scaled intervals.
    double scaled = std::floor(double(delta) * rate_);
    if (!std::isfinite(scaled) || scaled >= 9223372036854775808.0)
      throw std::overflow_error("time scale overflow");
    auto step = int64_t(scaled);
    if (domain_ > INT64_MAX - step)
      throw std::overflow_error("time domain overflow");
    return domain_ + step;
  }
  int64_t now(int64_t estimated_business) {
    auto value = at(estimated_business);
    if (value > high_)
      high_ = value;
    return high_;
  }
};
} // namespace roost
