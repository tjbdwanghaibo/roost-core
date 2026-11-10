#include "dao_sync.pb.h"
#include "roost/entity_views.hpp"
#include <algorithm>
#include <chrono>
#include <ctime>
#include <iostream>
#ifndef _WIN32
#include <sys/resource.h>
#endif
using bytes = roost::bytes;
static void varint(bytes &b, uint64_t n) {
  for (; n >= 0x80; n >>= 7)
    b.push_back(uint8_t(n | 0x80));
  b.push_back(uint8_t(n));
}
static void text(bytes &b, const std::string &s) {
  varint(b, s.size());
  b.insert(b.end(), s.begin(), s.end());
}
// Frame protocol 2 with subject envelope v3 (see roost/sync.hpp).
static bytes packet(uint32_t tick, bool full, bool create,
                    const std::vector<uint16_t> &ids,
                    std::vector<uint64_t> &versions) {
  bytes out = {0x52, 0x46, 2, uint8_t(full ? 1 : 2)};
  varint(out, 1);
  varint(out, 1);
  varint(out, tick);
  if (!full)
    varint(out, 1);
  varint(out, 1);
  varint(out, ids.size());
  for (auto id : ids) {
    uint64_t previous = versions[id]++;
    roost::dao::sync::SyncHeroDao dao;
    dao.set_entity_id(id);
    dao.set_sync_format(3);
    dao.mutable_level()->set_value(int(versions[id]));
    auto payload = dao.SerializeAsString();
    bytes body = {uint8_t(create ? 1 : 0)};
    if (create) {
      varint(body, id);
      varint(body, 1);
      varint(body, versions[id]);
      varint(body, 0);
      varint(body, 2);
      body.push_back(0);
      varint(body, 1);
      text(body, "hero");
      text(body, "");
    } else {
      varint(body, previous);
      varint(body, versions[id] - previous);
    }
    body.insert(body.end(), payload.begin(), payload.end());
    out.push_back(uint8_t(create ? 1 : 2));
    varint(out, id);
    varint(out, 1);
    if (create)
      varint(out, 1);
    varint(out, 1);
    out.push_back(1);
    varint(out, 1);
    varint(out, 1);
    varint(out, body.size());
    out.insert(out.end(), body.begin(), body.end());
  }
  return out;
}
int main(int argc, char **argv) {
  try {
    if (argc != 3)
      return 2;
    int count = std::stoi(argv[1]), percent = std::stoi(argv[2]);
    if (count < 1 || count > 10000 || percent < 1 || percent > 100)
      throw std::runtime_error("invalid load");
    roost::entity_views views;
    views.register_dao<roost::dao::sync::SyncHeroDao>("hero", "default", 1);
    std::vector<uint64_t> versions(count + 1);
    uint32_t sequence = 0;
    std::vector<roost::protobuf_change> changes;
    auto receive = [&](const bytes &data) {
      if (!views.receive(data.data(), data.size(), changes))
        throw std::runtime_error("frame rejected");
    };
    auto cold = std::chrono::steady_clock::now();
    for (int start = 1; start <= count; start += 200) {
      std::vector<uint16_t> ids;
      for (int i = start; i <= count && i < start + 200; ++i)
        ids.push_back(uint16_t(i));
      auto tick = ++sequence;
      receive(packet(tick, tick == 1, true, ids, versions));
    }
    double coldMs = std::chrono::duration<double, std::milli>(
                        std::chrono::steady_clock::now() - cold)
                        .count();
    std::vector<double> times;
    uint64_t size = 0;
    auto cpu = std::clock();
    for (int t = 0; t < 200; ++t) {
      int offset = t * count * percent / 100;
      int dirty = (t + 1) * count * percent / 100 - offset;
      std::vector<bytes> packets;
      for (int start = 0; start < dirty; start += 200) {
        std::vector<uint16_t> ids;
        for (int j = start; j < dirty && j < start + 200; ++j)
          ids.push_back(uint16_t((offset + j) % count + 1));
        packets.push_back(packet(++sequence, false, false, ids, versions));
        size += packets.back().size();
      }
      auto before = std::chrono::steady_clock::now();
      for (const auto &p : packets)
        receive(p);
      times.push_back(std::chrono::duration<double, std::micro>(
                          std::chrono::steady_clock::now() - before)
                          .count());
    }
    double cpuMs = 1000.0 * (std::clock() - cpu) / CLOCKS_PER_SEC;
    if (views.current().size() != size_t(count))
      throw std::runtime_error("visible count");
    for (const auto &entry : views.current()) {
      auto value = dynamic_cast<const roost::dao::sync::SyncHeroDao *>(
          entry.second.value.get());
      if (!value || value->level().value() != int(versions[entry.first.first]))
        throw std::runtime_error("final value mismatch");
    }
    std::sort(times.begin(), times.end());
    long rss = 0;
#ifndef _WIN32
    rusage usage{};
    getrusage(RUSAGE_SELF, &usage);
    rss = usage.ru_maxrss;
#ifndef __APPLE__
    rss *= 1024;
#endif
#endif
    std::cout << "{\"visible\":" << count << ",\"dirty_percent\":" << percent
              << ",\"ticks\":200,\"cold_ms\":" << coldMs
              << ",\"apply_p50_us\":" << times[99]
              << ",\"apply_p99_us\":" << times[197]
              << ",\"apply_max_us\":" << times.back()
              << ",\"cpu_ms_including_fixture\":" << cpuMs
              << ",\"sync_bytes_per_simulated_second\":" << size / 10
              << ",\"rss_high_bytes\":" << rss << ",\"verified\":true}\n";
  } catch (const std::exception &e) {
    std::cerr << e.what() << "\n";
    return 1;
  }
}
