#include "roost/client.hpp"
#include "roost/network_time.hpp"
#include "roost/sync.hpp"
#include <chrono>
#include <fstream>
#include <iostream>
#include <iterator>
int main(int argc, char **argv) {
  try {
    if (argc < 2)
      return 2;
    if (std::string(argv[1]) == "sync") {
      std::ifstream input(argv[2], std::ios::binary);
      roost::bytes data{std::istreambuf_iterator<char>(input), {}};
      roost::sync_receiver receiver;
      std::vector<roost::change> changes;
      if (!receiver.receive(data.data(), data.size(), changes) ||
          changes.empty() || changes[0].kind != roost::change_kind::full)
        throw std::runtime_error("full missing");
      if (receiver.receive(data.data(), data.size(), changes))
        throw std::runtime_error("duplicate applied");
      auto corrupt = data;
      corrupt[23]++;
      corrupt.pop_back();
      try {
        receiver.receive(corrupt.data(), corrupt.size(), changes);
        throw std::logic_error("truncated accepted");
      } catch (const std::runtime_error &) {
      }
      receiver.reset();
      if (!receiver.receive(data.data(), data.size(), changes))
        throw std::runtime_error("reset failed");
      auto write = [](roost::bytes &bytes, size_t at, uint64_t value,
                      size_t width) {
        for (size_t i = width; i > 0; --i) {
          bytes[at + i - 1] = uint8_t(value);
          value >>= 8;
        }
      };
      auto delta = data;
      delta[6] = 2;
      delta[32] = 2;
      write(delta, 20, 4, 4);
      write(delta, 24, 3, 4);
      write(delta, 56, 0, 2);
      write(delta, 70, 4, 8);
      write(delta, 78, 3, 8);
      if (!receiver.receive(delta.data(), delta.size(), changes) ||
          changes.size() != 1 || changes[0].kind != roost::change_kind::delta)
        throw std::runtime_error("delta missing");
      auto remove = roost::bytes(delta.begin(), delta.begin() + 41);
      remove[32] = 3;
      write(remove, 20, 5, 4);
      write(remove, 24, 4, 4);
      write(remove, 39, 0, 2);
      if (!receiver.receive(remove.data(), remove.size(), changes) ||
          changes.size() != 1 || changes[0].kind != roost::change_kind::remove)
        throw std::runtime_error("remove missing");
      receiver.reset();
      receiver.receive(data.data(), data.size(), changes);
      auto empty = roost::bytes(data.begin(), data.begin() + 32);
      write(empty, 20, 4, 4);
      write(empty, 30, 0, 2);
      receiver.receive(empty.data(), empty.size(), changes);
      if (changes.size() != 1 || !changes[0].snapshot_replacement)
        throw std::runtime_error("full replacement removal missing");
      std::cout << "PASS native Sync Go "
                   "golden/full/delta/remove/duplicate/malformed/reset\n";
      return 0;
    }
    auto options = roost_default_options();
    options.request_timeout_ms = 1000;
    options.heartbeat_ms = 20;
    options.max_requests = 2;
    roost::client client(options);
    bool ready = false, response = false, timeout = false, push = false;
    bool notified = false, canceled = false, second_phase = false;
    uint32_t cancel_sequence = 0;
    uint32_t seq = 0;
    bool clock_requested = false, clock_received = false;
    client.on_event = [&](const roost::client_event &e) {
      if (e.type == ROOST_READY)
        ready = true;
      if (e.type == ROOST_PUSH) {
        push = true;
        if (e.id == 10 && !client.cancel(cancel_sequence))
          throw std::runtime_error("cancel admission failed");
      }
      if (e.type == ROOST_CLOSED)
        throw std::runtime_error("unexpected close");
    };
    bool failed = false;
    client.on_callback_error = [&] { failed = true; };
    client.connect("127.0.0.1", uint16_t(std::stoi(argv[1])), "ticket");
    auto until = std::chrono::steady_clock::now() + std::chrono::seconds(5);
    while (std::chrono::steady_clock::now() < until) {
      client.pump();
      if (failed)
        throw std::runtime_error("callback failed");
      if (ready && !seq) {
        seq = client.request(42, 43, {8, 1}, [&](const roost::client_event &e) {
          response =
              e.type == ROOST_RESPONSE && e.payload == roost::bytes({8, 2});
        });
        client.request(
            44, 45, {},
            [&](const roost::client_event &e) {
              timeout = e.type == ROOST_REQUEST_FAILED &&
                        e.status == ROOST_TIMEOUT_UNKNOWN;
            },
            100);
        try {
          client.request(44, 45, {}, {});
          throw std::logic_error("unbounded requests");
        } catch (const std::runtime_error &) {
        }
      }
      if (response && timeout && push && !second_phase) {
        second_phase = true;
        client.notify(50001, 2, {0xc8, 1, 3, 1},
                      [&](const roost::client_event &e) {
                        notified = e.type == ROOST_SENT && e.kind == 2;
                      });
        cancel_sequence =
            client.request(46, 47, {}, [&](const roost::client_event &e) {
              canceled = e.type == ROOST_REQUEST_FAILED &&
                         e.status == ROOST_CANCELED_UNKNOWN;
            });
      }
      if (notified && canceled && !clock_requested) {
        clock_requested = true;
        double sent = roost::local_seconds();
        client.clock_probe([&, sent](const roost::client_event &e) {
          if (e.type != ROOST_RESPONSE)
            throw std::runtime_error("clock failed");
          auto sample = roost::clock_sample::decode(
              sent, roost::local_seconds(), e.payload.data(), e.payload.size());
          roost::network_clock clock;
          clock_received = clock.observe(sample);
        });
      }
      if (clock_received) {
        client.disconnect();
        client.pump();
        std::cout << "PASS native TCP/auth/PB/push/heartbeat/timeout/bounded "
                     "requests\n";
        return 0;
      }
      std::this_thread::sleep_for(std::chrono::milliseconds(1));
    }
    throw std::runtime_error("network test deadline");
  } catch (const std::exception &e) {
    std::cerr << e.what() << "\n";
    return 1;
  }
}
