#include "roost/lockstep.hpp"
#include "roost/lockstep.h"
#include "roost/time_domain.hpp"
#include <fstream>
#include <iostream>
#include <sstream>
using namespace roost;
static void check(bool ok, const char *why) {
  if (!ok)
    throw std::runtime_error(why);
}
template <class F> static void refuses(F call) {
  bool failed = false;
  try {
    call();
  } catch (const std::exception &) {
    failed = true;
  }
  check(failed, "invalid operation accepted");
}
static bytes frames(std::initializer_list<uint8_t> ids) {
  bytes b{0xc7, 1, uint8_t(ids.size())};
  for (auto id : ids) {
    b.push_back(id);
    b.push_back(0);
  }
  return b;
}
static bytes unhex(std::string text) {
  bytes b;
  for (size_t i = 0; i < text.size(); i += 2)
    b.push_back(uint8_t(std::stoul(text.substr(i, 2), nullptr, 16)));
  return b;
}
int main(int argc, char **argv) {
  try {
    check(argc == 2, "fixture required");
    std::ifstream file(argv[1]);
    std::map<std::string, bytes> golden;
    std::string name, hex;
    while (file >> name >> hex)
      golden[name] = unhex(hex);
    auto broadcast = decode_lockstep_broadcast(golden.at("lockstep"));
    check(broadcast.size() == 2 && broadcast[0].inputs[1].player == -1,
          "Go frame/player");
    lockstep_input_hasher hasher;
    for (auto &f : broadcast)
      hasher.apply(f);
    check(hasher.value() == 15941985346279483390ULL, "Go state hash");
    check(encode_lockstep_command(lockstep_operation::input, 1, {8, 42}) ==
              golden.at("lockstep_input"),
          "Go input");
    check(encode_lockstep_command(lockstep_operation::hash, 2, {},
                                  UINT64_MAX) == golden.at("lockstep_hash"),
          "Go hash");
    check(encode_lockstep_command(lockstep_operation::catchup, 1) ==
              golden.at("lockstep_catchup"),
          "Go catchup");
    for (const auto &entry : golden)
      if (entry.first.rfind("sha_", 0) == 0) {
        auto size = std::stoul(entry.first.substr(4));
        bytes data(size);
        for (size_t i = 0; i < size; ++i)
          data[i] = uint8_t(i * 31);
        auto digest = snapshot_digest(data);
        check(bytes(digest.begin(), digest.end()) == entry.second,
              "Python SHA256");
      }
    lockstep_assembler assembler(1);
    check(assembler.receive(frames({3})).frames.empty(), "future buffered");
    refuses([&] { assembler.receive(frames({4})); });
    check(assembler.next() == 1 && assembler.buffered() == 1,
          "overflow atomic");
    check(assembler.receive(frames({1, 2})).frames.size() == 3 &&
              assembler.next() == 4,
          "gap heals");
    check(assembler.receive(frames({1, 2, 3})).frames.empty(), "duplicate");
    for (auto bad : std::vector<bytes>{
             {0xc7, 1, 1, 0, 0},
             {0xc7, 1, 2, 2, 0, 1, 0},
             {0xc7, 1, 1, 1, 0, 0},
             {0xc7, 1, 65},
             {0xc7, 1, 1, 255, 255, 255, 255, 255, 255, 255, 255, 255, 2},
             {0xc7, 1, 1, 1, 1, 7, 2, 42},
             {0xc7, 1, 1, 1, 1, 7, 129, 8},
             {0xc7, 1, 1, 128, 128, 128, 128, 16, 0},
             {0xc7, 1, 1, 1, 129, 2}})
      refuses([&] { decode_lockstep_broadcast(bad); });
    for (auto key :
         {"catchup_unservable", "recovery_failed_snapshot_unavailable"}) {
      bool failed = false;
      try {
        assembler.receive(golden.at(key));
      } catch (const recovery_failure &e) {
        failed = e.reason >= 1 && e.reason <= 4;
      }
      check(failed, "Go recovery notice");
    }
    auto first = golden.at("snapshot_first_chunk"),
         last = golden.at("snapshot_next_chunk");
    check(!assembler.receive(first).snapshot &&
              assembler.snapshot_in_progress(),
          "partial snapshot");
    check(!assembler.receive(first).snapshot, "duplicate chunk");
    check(assembler.receive(frames({4})).frames.empty(), "frames paused");
    auto snapshot = assembler.receive(last).snapshot;
    check(snapshot && snapshot->frame == 1234 &&
              snapshot->state_hash == 9223372036854780468ULL &&
              std::string(snapshot->data.begin(), snapshot->data.end()) ==
                  "roost-snapshot-golden-24",
          "Go snapshot");
    refuses([&] { assembler.receive(frames({4})); });
    refuses([&] { assembler.resume_after_snapshot(1); });
    assembler.resume_after_snapshot(1234);
    check(assembler.next() == 1235 && !assembler.gap(), "resume snapshot");
    lockstep_assembler corrupt;
    corrupt.receive(first);
    auto broken = last;
    broken.back() ^= 1;
    refuses([&] { corrupt.receive(broken); });
    lockstep_assembler skip;
    skip.receive(first);
    auto jump = last;
    jump[3]++;
    refuses([&] { skip.receive(jump); });
    lockstep_assembler small(1, 8);
    refuses([&] { small.receive(first); });
    std::vector<bytes> sent;
    std::vector<uint32_t> applied;
    lockstep_client client(
        [&](const bytes &b) { sent.push_back(b); },
        [](const lockstep_snapshot &s) { return s.state_hash; }, 256, 2);
    auto simulate = [&](const lockstep_frame &f) { applied.push_back(f.id); };
    client.receive(frames({3}), simulate);
    check(sent.size() == 1 && sent[0] == golden.at("lockstep_catchup"),
          "gap request");
    client.receive(frames({3}), simulate);
    client.receive(frames({3}), simulate);
    check(sent.size() == 2, "gap retry");
    client.receive(frames({1, 2}), simulate);
    check(applied == std::vector<uint32_t>({1, 2, 3}), "ordered simulation");
    client.receive(first, simulate);
    client.receive(last, simulate);
    check(client.snapshots_restored() == 1 && client.next() == 1235,
          "restore callback");
    lockstep_client failing([](const bytes &) {});
    refuses([&] {
      failing.receive(frames({1, 2}), [](const lockstep_frame &) {
        throw std::runtime_error("business");
      });
    });
    refuses([&] { failing.submit_input(2, {}); });
    check(bool(failing.failure()), "simulation failure terminal");
    lockstep_client wrong([](const bytes &) {},
                          [](const lockstep_snapshot &) { return 0; });
    wrong.receive(first, simulate);
    refuses([&] { wrong.receive(last, simulate); });
    roost_lockstep_callbacks cb{};
    cb.context = &applied;
    cb.send = [](void *, const uint8_t *, size_t) { return 1; };
    cb.simulate = [](void *p, uint32_t f, const roost_lockstep_input *,
                     size_t) {
      static_cast<std::vector<uint32_t> *>(p)->push_back(f);
      return 1;
    };
    auto *c = roost_lockstep_create(cb, 256, 64, 4u << 20);
    check(c != nullptr, "C create");
    auto one = frames({1});
    check(roost_lockstep_receive(c, one.data(), one.size()) &&
              roost_lockstep_next(c) == 2,
          "C ABI frame");
    check(!roost_lockstep_submit(c, 0, nullptr, 0) && *roost_lockstep_error(c),
          "C ABI error");
    roost_lockstep_destroy(c);
    scene_time_domain time;
    check(time.at(100) == 100, "identity");
    time.update(100, 500, 0.5);
    check(time.at(110) == 505 && time.now(120) == 510 && time.now(110) == 510,
          "scale/monotonic");
    time.update(200, 700, 0);
    check(time.at(900) == 700, "pause");
    refuses([&] { time.update(1, 1, -1); });
    check(time.rate() == 0, "invalid update atomic");
    time.update(1, INT64_MAX, 1);
    refuses([&] { time.at(2); });
    time.update(INT64_MIN, INT64_MIN, 1);
    check(time.at(INT64_MAX) == INT64_MAX, "full signed range");
    std::cout << "PASS native Go Lockstep goldens, recovery, snapshot, SHA256, "
                 "C ABI, time domain\n";
  } catch (const std::exception &e) {
    std::cerr << e.what() << "\n";
    return 1;
  }
}
