#pragma once
#include <cstdint>
#include <deque>
#include <stdexcept>
#include <utility>
namespace roost {
// 输入值不可变；重放必须是纯模拟，不能发 RPC/落库/发奖。重连创建新历史。
template <class Input> class prediction_history {
  std::deque<std::pair<uint64_t, Input>> entries_;
  size_t capacity_;
  uint64_t sent_ = 0, acknowledged_ = 0, lost_ = 0;

public:
  explicit prediction_history(size_t capacity = 128) : capacity_(capacity) {
    if (!capacity || capacity > 65536)
      throw std::invalid_argument("prediction capacity");
  }
  bool held() const { return acknowledged_ < lost_; }
  size_t size() const { return entries_.size(); }
  const Input &operator[](size_t index) const {
    return entries_.at(index).second;
  }
  void record(uint64_t sequence, Input input) {
    if (sequence <= sent_)
      throw std::invalid_argument("prediction input order");
    if (entries_.size() == capacity_) {
      lost_ = entries_.front().first;
      entries_.pop_front();
    }
    entries_.emplace_back(sequence, std::move(input));
    sent_ = sequence;
  }
  bool acknowledge(uint64_t sequence, Input &last) {
    if (sequence < acknowledged_ || sequence > sent_)
      throw std::invalid_argument("prediction acknowledgement order");
    bool found = false;
    while (!entries_.empty() && entries_.front().first <= sequence) {
      last = entries_.front().second;
      entries_.pop_front();
      found = true;
    }
    acknowledged_ = sequence;
    return found;
  }
  template <class State, class Clone, class Simulate>
  State replay(const State &authority, Clone clone, Simulate simulate) const {
    State state = clone(authority);
    if (!held())
      for (const auto &entry : entries_)
        state = simulate(state, entry.second);
    return state;
  }
};
} // namespace roost
