#include "roost/lockstep.hpp"
#include <algorithm>
#include <limits>
namespace roost {
namespace {
struct reader {
  const bytes &data;
  size_t at = 0;
  uint8_t byte() {
    if (at == data.size())
      throw std::runtime_error("truncated Lockstep packet");
    return data[at++];
  }
  uint64_t varint() {
    uint64_t value = 0;
    for (int i = 0; i < 10; ++i) {
      uint8_t b = byte();
      if (i == 9 && b > 1)
        throw std::runtime_error("Lockstep varint overflow");
      value |= uint64_t(b & 127) << (7 * i);
      if (!(b & 128))
        return value;
    }
    throw std::runtime_error("Lockstep varint overflow");
  }
  uint32_t frame(bool zero = false) {
    auto v = varint();
    if (v > UINT32_MAX || (!zero && !v))
      throw std::runtime_error("invalid Lockstep frame");
    return uint32_t(v);
  }
  bytes take(size_t count) {
    if (count > data.size() - at)
      throw std::runtime_error("truncated Lockstep payload");
    bytes value(data.begin() + at, data.begin() + at + count);
    at += count;
    return value;
  }
  void end() {
    if (at != data.size())
      throw std::runtime_error("Lockstep trailing bytes");
  }
};
void write(bytes &out, uint64_t value) {
  do {
    uint8_t b = uint8_t(value & 127);
    value >>= 7;
    out.push_back(b | (value ? 128 : 0));
  } while (value);
}
} // namespace
bytes encode_lockstep_command(lockstep_operation operation, uint32_t frame,
                              const bytes &payload, uint64_t hash) {
  if (!frame || operation < lockstep_operation::input ||
      operation > lockstep_operation::catchup ||
      (operation == lockstep_operation::input &&
       (payload.size() > 1024 || hash)) ||
      (operation != lockstep_operation::input && !payload.empty()) ||
      (operation == lockstep_operation::catchup && hash))
    throw std::invalid_argument("invalid Lockstep command");
  bytes out{0xc8, 1, uint8_t(operation)};
  write(out, frame);
  if (operation == lockstep_operation::input) {
    write(out, payload.size());
    out.insert(out.end(), payload.begin(), payload.end());
  }
  if (operation == lockstep_operation::hash)
    write(out, hash);
  return out;
}
std::vector<lockstep_frame> decode_lockstep_broadcast(const bytes &packet) {
  reader r{packet};
  if (r.byte() != 0xc7 || r.byte() != 1)
    throw std::runtime_error("invalid Lockstep broadcast header");
  auto count = r.varint();
  if (count > 64)
    throw std::runtime_error("too many Lockstep frames");
  std::vector<lockstep_frame> frames;
  uint32_t last = 0;
  for (uint64_t i = 0; i < count; ++i) {
    lockstep_frame frame{r.frame(), {}};
    if (frame.id <= last)
      throw std::runtime_error("Lockstep frames must increase");
    last = frame.id;
    auto inputs = r.varint();
    if (inputs > 256)
      throw std::runtime_error("too many Lockstep inputs");
    for (uint64_t j = 0; j < inputs; ++j) {
      auto player = r.varint(), length = r.varint();
      if (player > UINT32_MAX || length > 1024)
        throw std::runtime_error("invalid Lockstep input");
      // Go wire uses uint32(int32), including negative player IDs.
      int64_t signed_player = player <= INT32_MAX
                                  ? int64_t(player)
                                  : int64_t(player) - (int64_t(1) << 32);
      frame.inputs.push_back({int32_t(signed_player), r.take(size_t(length))});
    }
    frames.push_back(std::move(frame));
  }
  r.end();
  return frames;
}
lockstep_assembler::lockstep_assembler(size_t buffered, size_t snapshot)
    : max_buffered_(buffered), max_snapshot_(snapshot) {
  if (!buffered || !snapshot || snapshot > (64u << 20))
    throw std::invalid_argument("invalid Lockstep capacity");
}
void lockstep_assembler::reset_transfer() {
  transfer_ = 0;
  receiving_.reset();
  received_ = 0;
}
std::optional<lockstep_snapshot>
lockstep_assembler::accept_chunk(const bytes &packet) {
  try {
    reader r{packet};
    if (r.byte() != 0xca || r.byte() != 1)
      throw std::runtime_error("invalid snapshot header");
    auto transfer = r.varint(), offset = r.varint();
    if (!transfer || offset >= max_snapshot_)
      throw std::runtime_error("invalid snapshot transfer/offset");
    lockstep_snapshot header;
    size_t total = 0;
    std::array<uint8_t, 32> digest{};
    if (!offset) {
      header.frame = r.frame(true);
      auto length = r.varint();
      if (!length || length > max_snapshot_)
        throw std::runtime_error("snapshot exceeds budget");
      total = size_t(length);
      auto flags = r.byte();
      if (flags > 1)
        throw std::runtime_error("invalid snapshot flags");
      header.has_state_hash = flags == 1;
      if (header.has_state_hash)
        header.state_hash = r.varint();
      auto sum = r.take(32);
      std::copy(sum.begin(), sum.end(), digest.begin());
      if (packet.size() - r.at > total)
        throw std::runtime_error("snapshot first chunk exceeds total");
    }
    const auto count = packet.size() - r.at;
    if (!count)
      throw std::runtime_error("empty snapshot chunk");
    if (transfer != transfer_) {
      if (offset)
        throw std::runtime_error("snapshot must start at zero");
      header.data.resize(total);
      receiving_ = std::move(header);
      transfer_ = transfer;
      received_ = 0;
      digest_ = digest;
    } else {
      if (!receiving_ || offset < received_)
        return {};
      if (offset > received_)
        throw std::runtime_error("snapshot offset gap");
    }
    if (count > receiving_->data.size() - received_)
      throw std::runtime_error("snapshot chunk overruns total");
    std::copy(packet.begin() + r.at, packet.end(),
              receiving_->data.begin() + received_);
    received_ += count;
    if (received_ != receiving_->data.size())
      return {};
    auto snapshot = std::move(*receiving_);
    receiving_.reset();
    received_ = 0;
    if (snapshot_digest(snapshot.data) != digest_)
      throw std::runtime_error("snapshot SHA-256 mismatch");
    unapplied_ = snapshot.frame;
    return snapshot;
  } catch (...) {
    reset_transfer();
    throw;
  }
}
lockstep_delivery lockstep_assembler::receive(const bytes &packet) {
  if (unapplied_)
    throw std::logic_error("apply pending snapshot before receiving");
  if (!packet.empty() && packet[0] == 0xc9) {
    reader r{packet};
    r.byte();
    if (r.byte() != 2)
      throw std::runtime_error("invalid recovery notice version");
    auto reason = r.varint();
    auto from = r.frame();
    auto start = r.frame(true);
    r.end();
    if (reason < 1 || reason > 4 || (reason == 1 && from >= start))
      throw std::runtime_error("invalid recovery notice");
    throw recovery_failure(uint32_t(reason), from, start);
  }
  if (!packet.empty() && packet[0] == 0xca)
    return {{}, accept_chunk(packet)};
  auto frames = decode_lockstep_broadcast(packet);
  if (receiving_)
    return {};
  auto pending = buffered_;
  auto next = next_;
  lockstep_delivery result;
  for (auto &frame : frames) {
    if (frame.id < next || pending.count(frame.id))
      continue;
    if (frame.id == next) {
      result.frames.push_back(std::move(frame));
      ++next;
      while (next <= UINT32_MAX) {
        auto found = pending.find(uint32_t(next));
        if (found == pending.end())
          break;
        result.frames.push_back(std::move(found->second));
        pending.erase(found);
        ++next;
      }
    } else {
      if (pending.size() == max_buffered_)
        throw std::runtime_error("Lockstep reorder budget exhausted");
      pending.emplace(frame.id, std::move(frame));
    }
  }
  buffered_ = std::move(pending);
  next_ = next;
  return result;
}
void lockstep_assembler::resume_after_snapshot(uint32_t frame) {
  if (!unapplied_ || *unapplied_ != frame)
    throw std::logic_error("snapshot frame does not match pending recovery");
  unapplied_.reset();
  buffered_.clear();
  next_ = uint64_t(frame) + 1;
}
uint64_t lockstep_input_hasher::apply(const lockstep_frame &frame) {
  uint64_t sum = 14695981039346656037ULL;
  auto feed = [&](uint8_t b) { sum = (sum ^ b) * 1099511628211ULL; };
  auto number = [&](uint64_t v) {
    for (int i = 0; i < 8; ++i) {
      feed(uint8_t(v));
      v >>= 8;
    }
  };
  number(value_);
  number(frame.id);
  for (const auto &input : frame.inputs) {
    number(uint64_t(int64_t(input.player)));
    number(input.payload.size());
    for (auto b : input.payload)
      feed(b);
  }
  return value_ = sum;
}
lockstep_client::lockstep_client(sender send, restore restore, size_t buffered,
                                 size_t retry, size_t snapshot)
    : send_(std::move(send)), restore_(std::move(restore)),
      assembler_(buffered, snapshot), retry_packets_(retry) {
  if (!send_ || !retry)
    throw std::invalid_argument("Lockstep sender/retry required");
}
void lockstep_client::check() const {
  if (failure_)
    std::rethrow_exception(failure_);
}
void lockstep_client::submit_input(uint32_t frame, const bytes &payload) {
  check();
  send_(encode_lockstep_command(lockstep_operation::input, frame, payload));
}
void lockstep_client::report_hash(uint32_t frame, uint64_t hash) {
  check();
  send_(encode_lockstep_command(lockstep_operation::hash, frame, {}, hash));
}
void lockstep_client::request_catchup(uint32_t frame) {
  check();
  send_(encode_lockstep_command(lockstep_operation::catchup, frame));
}
void lockstep_client::receive(
    const bytes &packet,
    const std::function<void(const lockstep_frame &)> &simulate) {
  check();
  if (consuming_ || !simulate)
    throw std::logic_error("Lockstep requires serial simulation");
  consuming_ = true;
  struct reset {
    bool &v;
    ~reset() { v = false; }
  } finish{consuming_};
  try {
    if (!restore_ && !packet.empty() && packet[0] == 0xca)
      throw std::runtime_error("snapshot restore callback required");
    auto delivery = assembler_.receive(packet);
    if (delivery.snapshot) {
      auto &snapshot = *delivery.snapshot;
      auto hash = restore_(snapshot);
      if (snapshot.has_state_hash && hash != snapshot.state_hash)
        throw std::runtime_error("restored state hash mismatch");
      assembler_.resume_after_snapshot(snapshot.frame);
      requested_ = stagnant_ = 0;
      ++restored_;
    }
    for (const auto &frame : delivery.frames)
      simulate(frame);
    if (assembler_.snapshot_in_progress())
      return;
    if (!assembler_.gap()) {
      requested_ = stagnant_ = 0;
      return;
    }
    if (!requested_ ||
        (requested_ == next() && ++stagnant_ >= retry_packets_)) {
      request_catchup(uint32_t(next()));
      requested_ = next();
      stagnant_ = 0;
    } else if (requested_ != next()) {
      requested_ = next();
      stagnant_ = 0;
    }
  } catch (...) {
    failure_ = std::current_exception();
    throw;
  }
}
} // namespace roost
