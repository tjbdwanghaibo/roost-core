#include "roost/game_client.hpp"
#include <iostream>
struct message {
  std::vector<uint8_t> data;
  bool SerializeToString(std::string *s) const {
    s->assign(data.begin(), data.end());
    return true;
  }
  bool ParseFromArray(const void *p, int n) {
    if (n < 0)
      return false;
    const auto *b = static_cast<const uint8_t *>(p);
    data.assign(b, b + n);
    return true;
  }
};
int main(int argc, char **argv) {
  try {
    if (argc != 2)
      return 2;
    roost::game_client game;
    int ready = 0, reconnected = 0, requests = 0, pushes = 0, frames = 0,
        attempts = 0, restored = 0;
    bool failed = false, acknowledged = false;
    int clocks = 0;
    std::unique_ptr<roost::lockstep_client> lockstep;
    game.on_error = [&](std::exception_ptr e) {
      failed = true;
      try {
        if (e)
          std::rethrow_exception(e);
      } catch (const std::exception &e) {
        std::cerr << e.what() << "\n";
      }
    };
    game.configure_reconnect(
        {true, 2, std::chrono::milliseconds(1), std::chrono::milliseconds(2)});
    auto ticket = roost::connection_ticket{"127.0.0.1", "ticket",
                                           uint16_t(std::stoi(argv[1]))};
    game.reconnect_ticket = [&](size_t) {
      return std::optional<roost::connection_ticket>{ticket};
    };
    game.on_reconnect_attempt = [&](size_t) { ++attempts; };
    game.on_reconnected = [&] { ++reconnected; };
    game.subscribe<message>(9, [&](const message &m) {
      if (m.data != roost::bytes({8, 3}))
        throw std::runtime_error("push mismatch");
      ++pushes;
    });
    game.on_ready = [&] {
      game.probe_clock(
          [&](roost::clock_sample sample) {
            roost::network_clock clock;
            if (!clock.observe(sample))
              throw std::runtime_error("clock sample");
            ++clocks;
          },
          [&](const roost::client_event &) { failed = true; });
      ++ready;
      lockstep = std::make_unique<roost::lockstep_client>(
          [&](const roost::bytes &b) {
            game.transport().notify(50001, 2, b,
                                    [&](const roost::client_event &e) {
                                      if (e.type != ROOST_SENT)
                                        failed = true;
                                    });
          },
          [&](const roost::lockstep_snapshot &snapshot) {
            if (std::string(snapshot.data.begin(), snapshot.data.end()) !=
                "roost-snapshot-golden-24")
              throw std::runtime_error("snapshot bytes");
            ++restored;
            return 9223372036854780468ULL;
          });
      game.bind_lockstep(50002, [&](const roost::bytes &b) {
        lockstep->receive(b, [&](const roost::lockstep_frame &) { ++frames; });
      });
      game.request<message>(
          42, 43, message{{8, 1}},
          [&](const message &m) {
            if (m.data != roost::bytes({8, 2}))
              throw std::runtime_error("response mismatch");
            ++requests;
          },
          [&](const roost::client_event &) { failed = true; });
      lockstep->submit_input(1, {8, 42});
    };
    game.connect(ticket);
    auto deadline = std::chrono::steady_clock::now() + std::chrono::seconds(5);
    while (std::chrono::steady_clock::now() < deadline) {
      game.pump();
      if (failed)
        throw std::runtime_error("game callback failed");
      if (!acknowledged && requests == 1 && pushes == 1 && frames == 2 &&
          restored == 1) {
        acknowledged = true;
        game.transport().notify(99, 0, {});
      }
      if (ready == 2 && reconnected == 1 && requests == 2 && pushes == 2 &&
          frames == 4 && attempts == 1 && restored == 2 && clocks == 2) {
        game.disconnect();
        std::cout << "PASS game PB routes, native Lockstep, reconnect without "
                     "replay\n";
        return 0;
      }
      std::this_thread::sleep_for(std::chrono::milliseconds(1));
    }
    throw std::runtime_error("game loop deadline");
  } catch (const std::exception &e) {
    std::cerr << e.what() << "\n";
    return 1;
  }
}
