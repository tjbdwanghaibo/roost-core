#pragma once
#include "client.hpp"
#include "sync.hpp"
#include <array>
#include <exception>
#include <optional>

namespace roost {
struct lockstep_input {
  int32_t player;
  bytes payload;
};
struct lockstep_frame {
  uint32_t id;
  std::vector<lockstep_input> inputs;
};
struct lockstep_snapshot {
  uint32_t frame = 0;
  bytes data;
  uint64_t state_hash = 0;
  bool has_state_hash = false;
};
struct lockstep_delivery {
  std::vector<lockstep_frame> frames;
  std::optional<lockstep_snapshot> snapshot;
};
class ROOST_API recovery_failure : public std::runtime_error {
public:
  uint32_t reason, requested_from, history_start;
  recovery_failure(uint32_t reason, uint32_t from, uint32_t start)
      : std::runtime_error("server cannot recover Lockstep match"),
        reason(reason), requested_from(from), history_start(start) {}
};
enum class lockstep_operation : uint8_t { input = 1, hash = 2, catchup = 3 };
ROOST_API bytes encode_lockstep_command(lockstep_operation operation,
                                        uint32_t frame,
                                        const bytes &payload = {},
                                        uint64_t hash = 0);
ROOST_API std::vector<lockstep_frame>
decode_lockstep_broadcast(const bytes &packet);
// 仅作快照完整性校验；认证仍由传输/账号层负责。
ROOST_API std::array<uint8_t, 32> snapshot_digest(const bytes &data);

// 一场比赛一个实例，仅消费线程使用。坏包不推进帧基线，快照恢复后显式继续。
class ROOST_API lockstep_assembler {
  size_t max_buffered_, max_snapshot_;
  std::map<uint32_t, lockstep_frame> buffered_;
  uint64_t next_ = 1, transfer_ = 0;
  std::optional<lockstep_snapshot> receiving_;
  std::optional<uint32_t> unapplied_;
  size_t received_ = 0;
  std::array<uint8_t, 32> digest_{};
  void reset_transfer();
  std::optional<lockstep_snapshot> accept_chunk(const bytes &packet);

public:
  explicit lockstep_assembler(size_t max_buffered = 256,
                              size_t max_snapshot = 4u << 20);
  uint64_t next() const { return next_; }
  size_t buffered() const { return buffered_.size(); }
  bool gap() const { return !buffered_.empty(); }
  bool snapshot_in_progress() const { return receiving_.has_value(); }
  bool snapshot_pending() const { return unapplied_.has_value(); }
  lockstep_delivery receive(const bytes &packet);
  void resume_after_snapshot(uint32_t frame);
};
class ROOST_API lockstep_input_hasher {
  uint64_t value_ = 14695981039346656037ULL;

public:
  uint64_t value() const { return value_; }
  void reset(uint64_t value) { value_ = value; }
  uint64_t apply(const lockstep_frame &frame);
};

// 不拥有网络连接。发送回调必须检查准入/写出失败并交给业务；模拟和恢复始终在消费线程。
class ROOST_API lockstep_client {
public:
  using sender = std::function<void(const bytes &)>;
  using restore = std::function<uint64_t(const lockstep_snapshot &)>;

private:
  sender send_;
  restore restore_;
  lockstep_assembler assembler_;
  uint64_t requested_ = 0;
  size_t stagnant_ = 0, retry_packets_, restored_ = 0;
  bool consuming_ = false;
  std::exception_ptr failure_;
  void check() const;

public:
  lockstep_client(sender send, restore apply_snapshot = {},
                  size_t max_buffered = 256, size_t retry_packets = 64,
                  size_t max_snapshot = 4u << 20);
  uint64_t next() const { return assembler_.next(); }
  size_t snapshots_restored() const { return restored_; }
  bool snapshot_in_progress() const {
    return assembler_.snapshot_in_progress();
  }
  std::exception_ptr failure() const { return failure_; }
  void submit_input(uint32_t frame, const bytes &payload);
  void report_hash(uint32_t frame, uint64_t hash);
  void request_catchup(uint32_t frame);
  void receive(const bytes &packet,
               const std::function<void(const lockstep_frame &)> &simulate);
};
} // namespace roost
