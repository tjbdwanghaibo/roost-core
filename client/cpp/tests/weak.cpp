#include "roost/client.hpp"
#include <chrono>
#include <iostream>
int main(int argc, char **argv) {
  try {
    if (argc != 3)
      return 2;
    std::string mode = argv[1];
    auto options = roost::default_options();
    options.max_events = 4;
    if (mode == "send_stall")
      options.max_payload = 16u << 20;
    options.request_timeout_ms = 200;
    options.heartbeat_ms = mode == "heartbeat" ? 20 : 0;
    options.heartbeat_timeout_ms = 100;
    roost::client client(options);
    bool ready = false, done = false, failed = false, sent = false;
    std::string reason;
    client.on_callback_error = [&] { failed = true; };
    client.on_event = [&](const roost::client_event &e) {
      if (mode == "truncated" && e.type == ROOST_PUSH)
        throw std::runtime_error("truncated payload was delivered");
      if (e.type == ROOST_READY)
        ready = true;
      if (e.type == ROOST_CLOSED) {
        reason.assign(e.payload.begin(), e.payload.end());
        done = true;
      }
    };
    client.connect("127.0.0.1", uint16_t(std::stoi(argv[2])), "ticket");
    auto deadline = std::chrono::steady_clock::now() + std::chrono::seconds(5);
    while (std::chrono::steady_clock::now() < deadline) {
      client.pump();
      if (failed)
        throw std::runtime_error("callback failure");
      if (ready && !sent) {
        sent = true;
        if (mode == "overflow")
          std::this_thread::sleep_for(std::chrono::milliseconds(300));
        if (mode == "send_stall")
          client.request(42, 43, std::vector<uint8_t>(16u << 20, 42), {});
        if (mode == "timeout")
          client.request(42, 43, {}, [&](const roost::client_event &e) {
            if (e.status != ROOST_TIMEOUT_UNKNOWN)
              throw std::runtime_error("wrong timeout classification");
            done = true;
          });
      }
      if (done) {
        if (mode == "send_stall" &&
            reason.find("socket send timeout") == std::string::npos)
          throw std::runtime_error(reason);
        if (mode == "heartbeat" &&
            reason.find("heartbeat timeout") == std::string::npos)
          throw std::runtime_error(reason);
        if (mode == "overflow" && reason.find("overflow") == std::string::npos)
          throw std::runtime_error(reason);
        std::cout << "PASS weak " << mode << " " << reason << "\n";
        return 0;
      }
      std::this_thread::sleep_for(std::chrono::milliseconds(1));
    }
    throw std::runtime_error("weak scenario deadline");
  } catch (const std::exception &e) {
    std::cerr << e.what() << "\n";
    return 1;
  }
}
