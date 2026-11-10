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
    // Unsigned varint, shortest encoding only, at most limit (frame protocol 2).
    uint64_t varint(uint64_t limit) {
      uint64_t v = 0;
      size_t start = at;
      for (unsigned shift = 0;; shift += 7) {
        if (at >= size || shift > 63)
          throw std::runtime_error("truncated Sync varint");
        uint8_t b = p[at++];
        if (shift == 63 && b > 1)
          throw std::runtime_error("overlong Sync varint");
        v |= uint64_t(b & 0x7f) << shift;
        if (b < 0x80)
          break;
      }
      size_t n = 1;
      for (uint64_t x = v; x >= 0x80; x >>= 7)
        ++n;
      if (v > limit || at - start != n)
        throw std::runtime_error("malformed Sync varint");
      return v;
    }
  };
  using key = std::pair<uint16_t, uint16_t>;
  std::map<uint16_t, uint16_t> objects_;
  std::map<key, subject> subjects_;
  uint64_t stream_ = 0;
  uint32_t epoch_ = 0, tick_ = 0;
  uint16_t schema_ = 0;
  bool needs_full_ = true;
  // Subject envelope v3: flags(1) (bit0 full, bit1 mask, bit2 reason).
  // Full: varint id, kind, version, base, [mask], [reason], encoding, lod(1),
  //       varint schema, varint length + namespace, varint length + profile
  //       ("default" travels as empty), payload.
  // Delta: varint base, version - base (>= 1), [mask], [reason], payload; it
  //       carries no identity and inherits it from the baseline of the same
  //       object and component, whose version must equal base.
  static subject decode(const bytes &b, const subject *baseline) {
    reader r{b.data(), b.size()};
    subject s;
    auto flags = r.number(1);
    if (flags & ~uint64_t(7))
      throw std::runtime_error("bad subject flags");
    s.full = flags & 1;
    auto optional = [&] {
      if (flags & 2) {
        s.mask = r.varint(UINT64_MAX);
        if (!s.mask)
          throw std::runtime_error("bad subject mask");
      }
      if ((flags & 4) && !r.varint(UINT32_MAX))
        throw std::runtime_error("bad subject reason");
    };
    if (s.full) {
      s.id = int64_t(r.varint(UINT64_MAX));
      s.kind = uint32_t(r.varint(UINT32_MAX));
      s.version = r.varint(UINT64_MAX);
      s.base = r.varint(UINT64_MAX);
      optional();
      s.encoding = uint16_t(r.varint(UINT16_MAX));
      s.lod = uint8_t(r.number(1));
      s.schema = uint32_t(r.varint(UINT32_MAX));
      s.name = r.text(size_t(r.varint(UINT16_MAX)));
      s.profile = r.text(size_t(r.varint(UINT16_MAX)));
      if (s.profile.empty())
        s.profile = "default";
      if (!s.id)
        throw std::runtime_error("bad subject id");
    } else {
      if (!baseline)
        throw std::runtime_error("missing subject baseline");
      s.base = r.varint(UINT64_MAX);
      auto step = r.varint(UINT64_MAX - s.base);
      if (!step || s.base != baseline->version)
        throw std::runtime_error("subject version/identity gap");
      s.version = s.base + step;
      optional();
      s.id = baseline->id;
      s.kind = baseline->kind;
      s.name = baseline->name;
      s.profile = baseline->profile;
      s.encoding = baseline->encoding;
      s.schema = baseline->schema;
      s.lod = baseline->lod;
    }
    s.payload = r.take(r.size - r.at);
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
      // Frame protocol 2: magic "RF"(2) protocol(1) kind(1), then varints:
      // stream, epoch, tick, [delta: tick - base], schema, object count.
      reader r{data, size};
      if (r.number(2) != 0x5246 || r.number(1) != 2)
        throw std::runtime_error("bad Sync header");
      auto flags = r.number(1);
      if (flags != 1 && flags != 2)
        throw std::runtime_error("bad Sync flags");
      bool full = flags == 1;
      auto stream = r.varint(UINT64_MAX);
      auto epoch = uint32_t(r.varint(UINT32_MAX)),
           tick = uint32_t(r.varint(UINT32_MAX));
      uint32_t base = full ? 0 : tick - uint32_t(r.varint(tick));
      auto schema = uint16_t(r.varint(UINT16_MAX));
      auto count = r.varint(UINT16_MAX);
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
        auto id = uint16_t(r.varint(UINT16_MAX)),
             gen = uint16_t(r.varint(UINT16_MAX));
        uint64_t archetype = op == 1 ? r.varint(UINT16_MAX) : 0;
        uint64_t components = op == 3 ? 0 : r.varint(UINT16_MAX);
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
          auto type = uint16_t(r.varint(UINT16_MAX));
          uint64_t version = 0, length = 0;
          if (operation == 1) {
            version = r.varint(UINT16_MAX);
            length = r.varint(UINT32_MAX);
          }
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
          // A delta inherits its identity from the baseline and must continue
          // its version (decode checks both).
          auto update = decode(r.take(size_t(length)),
                               old == next.end() ? nullptr : &old->second);
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
