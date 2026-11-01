#pragma once
#include <cstdint>
#include <map>
#include <set>
#include <stdexcept>
#include <string>
#include <utility>
#include <vector>

namespace roost {
using bytes = std::vector<uint8_t>;
enum class change_kind { full, delta, remove };
struct subject {
  int64_t id = 0;
  uint32_t kind = 0, schema = 0;
  uint64_t version = 0, base = 0, mask = 0;
  uint16_t encoding = 0;
  uint8_t lod = 0;
  bool full = false;
  std::string name, profile;
  bytes payload;
};
struct change {
  change_kind kind;
  uint16_t object = 0, generation = 0, component = 0;
  subject update;
  bool snapshot_replacement = false;
};
// Packet/application limits bound allocations before handing payloads to
// protobuf.
class sync_receiver {
  struct reader {
    const uint8_t *p;
    size_t size, at = 0;
    uint64_t number(size_t n) {
      if (n > size - at)
        throw std::runtime_error("truncated Sync");
      uint64_t v = 0;
      for (size_t i = 0; i < n; ++i)
        v = v << 8 | p[at++];
      return v;
    }
    bytes take(size_t n) {
      if (n > size - at)
        throw std::runtime_error("truncated Sync payload");
      bytes b(p + at, p + at + n);
      at += n;
      return b;
    }
    std::string text(size_t n) {
      auto b = take(n);
      return std::string(b.begin(), b.end());
    }
  };
  using key = std::pair<uint16_t, uint16_t>;
  std::map<uint16_t, uint16_t> objects_;
  std::map<key, subject> subjects_;
  uint64_t stream_ = 0;
  uint32_t epoch_ = 0, tick_ = 0;
  uint16_t schema_ = 0;
  bool needs_full_ = true;
  static subject decode(const bytes &b) {
    reader r{b.data(), b.size()};
    subject s;
    if (r.number(4) != 0x52535355 || r.number(2) != 2)
      throw std::runtime_error("bad subject header");
    auto flags = r.number(2);
    if (flags > 1)
      throw std::runtime_error("bad subject flags");
    s.full = flags == 1;
    s.id = int64_t(r.number(8));
    s.kind = uint32_t(r.number(4));
    s.version = r.number(8);
    s.base = r.number(8);
    s.mask = r.number(8);
    r.number(4);
    s.encoding = uint16_t(r.number(2));
    s.lod = uint8_t(r.number(1));
    if (r.number(1))
      throw std::runtime_error("bad subject reserved byte");
    s.schema = uint32_t(r.number(4));
    auto ns = r.number(2), profile = r.number(2), length = r.number(4);
    s.name = r.text(size_t(ns));
    s.profile = r.text(size_t(profile));
    s.payload = r.take(size_t(length));
    if (s.profile.empty())
      s.profile = "default";
    if (!s.id || r.at != r.size)
      throw std::runtime_error("bad subject length/id");
    return s;
  }

public:
  uint32_t epoch() const { return epoch_; }
  uint32_t tick() const { return tick_; }
  void reset() {
    objects_.clear();
    subjects_.clear();
    stream_ = 0;
    epoch_ = tick_ = schema_ = 0;
    needs_full_ = true;
  }
  // The returned list contains PB field patches, not merged application
  // objects. Caller applies every patch atomically before notifying engine
  // objects. On codec error reset/disconnect.
  bool receive(const uint8_t *data, size_t size, std::vector<change> &changes) {
    changes.clear();
    try {
      if (!data || size > 4u * 1024 * 1024)
        throw std::runtime_error("Sync size limit");
      reader r{data, size};
      if (r.number(4) != 0x43525031 || r.number(2) != 1)
        throw std::runtime_error("bad Sync header");
      auto flags = r.number(1);
      if ((flags != 1 && flags != 2) || r.number(1))
        throw std::runtime_error("bad Sync flags");
      bool full = flags == 1;
      auto stream = r.number(8);
      auto epoch = uint32_t(r.number(4)), tick = uint32_t(r.number(4)),
           base = uint32_t(r.number(4));
      auto schema = uint16_t(r.number(2));
      auto count = r.number(2);
      if (!stream || !epoch || !tick || !schema ||
          (full ? base != 0 : base == 0 || base >= tick) || count > 200)
        throw std::runtime_error("bad Sync baseline");
      if (stream_ && stream_ != stream)
        throw std::runtime_error("stream changed without reset");
      if (stream_ && (epoch < epoch_ || (epoch == epoch_ && tick <= tick_)))
        return false;
      if (!full && (needs_full_ || epoch != epoch_ || base != tick_ ||
                    schema != schema_))
        throw std::runtime_error("Sync gap");
      auto ids = full ? decltype(objects_){} : objects_;
      auto next = full ? decltype(subjects_){} : subjects_;
      std::vector<change> result;
      std::set<std::pair<uint16_t, uint16_t>> seen;
      for (uint64_t i = 0; i < count; ++i) {
        auto op = r.number(1);
        auto id = uint16_t(r.number(2)), gen = uint16_t(r.number(2));
        auto archetype = r.number(2), components = r.number(2);
        if (!id || !gen || op < 1 || op > 3 || (full && op != 1) ||
            (op == 1 && !archetype) || (op == 3 && components) ||
            components > 128 || !seen.emplace(id, gen).second)
          throw std::runtime_error("bad Sync object");
        if (op == 1) {
          if (ids.count(id) || ids.size() >= 10000)
            throw std::runtime_error("object conflict/capacity");
          ids[id] = gen;
        } else if (!ids.count(id) || ids.at(id) != gen)
          throw std::runtime_error("object generation gap");
        if (op == 3) {
          ids.erase(id);
          for (auto it = next.begin(); it != next.end();) {
            if (it->first.first == id) {
              result.push_back({change_kind::remove, id, gen, it->first.second,
                                it->second, false});
              it = next.erase(it);
            } else
              ++it;
          }
          continue;
        }
        std::set<uint16_t> components_seen;
        for (uint64_t c = 0; c < components; ++c) {
          auto operation = r.number(1);
          auto type = uint16_t(r.number(2));
          auto version = r.number(2), length = r.number(4);
          if (!type || !components_seen.insert(type).second || operation < 1 ||
              operation > 2 || length > 65536 ||
              (operation == 1 && version != 1) ||
              (operation == 2 && (version || length)))
            throw std::runtime_error("bad Sync component");
          key k{id, type};
          auto old = next.find(k);
          if (operation == 2) {
            if (old != next.end()) {
              result.push_back(
                  {change_kind::remove, id, gen, type, old->second, false});
              next.erase(old);
            }
            continue;
          }
          auto update = decode(r.take(size_t(length)));
          if (!update.full) {
            if (old == next.end())
              throw std::runtime_error("missing subject baseline");
            const auto &previous = old->second;
            if (previous.id != update.id || previous.kind != update.kind ||
                previous.name != update.name ||
                previous.profile != update.profile ||
                previous.encoding != update.encoding ||
                previous.schema != update.schema ||
                previous.lod != update.lod || previous.version != update.base ||
                update.version <= update.base)
              throw std::runtime_error("subject version/identity gap");
          }
          result.push_back(
              {update.full ? change_kind::full : change_kind::delta, id, gen,
               type, update, false});
          // Baseline metadata only; do not retain every historical patch
          // payload.
          update.payload.clear();
          next[k] = std::move(update);
        }
      }
      if (r.at != r.size)
        throw std::runtime_error("trailing Sync bytes");
      if (full) {
        std::vector<change> removed;
        for (const auto &item : subjects_) {
          auto id = item.first.first;
          if (!next.count(item.first) || objects_.at(id) != ids.at(id))
            removed.push_back({change_kind::remove, id, objects_.at(id),
                               item.first.second, item.second, true});
        }
        result.insert(result.begin(), removed.begin(), removed.end());
      }
      objects_ = std::move(ids);
      subjects_ = std::move(next);
      stream_ = stream;
      epoch_ = epoch;
      tick_ = tick;
      schema_ = schema;
      needs_full_ = false;
      changes = std::move(result);
      return true;
    } catch (...) {
      needs_full_ = true;
      throw;
    }
  }
};
} // namespace roost
