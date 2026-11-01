#include "roost/client.h"
#include <atomic>
#include <chrono>
#include <iostream>
#include <string>
#include <thread>

// C ABI 支持应用线程准入，网络线程同时发心跳；对端按正式 Gate
// 规则检查线上序号。
int main(int argc, char **argv) {
  if (argc != 2)
    return 2;
  auto options = roost_default_options();
  options.heartbeat_ms = 1;
  options.request_timeout_ms = 5000;
  auto *client = roost_create(options);
  if (!client)
    return 2;
  const uint8_t ticket[] = {'t'};
  if (!roost_connect(client, "127.0.0.1", uint16_t(std::stoi(argv[1])), ticket,
                     1)) {
    roost_destroy(client);
    return 2;
  }
  std::atomic<bool> ready{false}, stop{false};
  std::thread application([&] {
    while (!ready && !stop)
      std::this_thread::sleep_for(std::chrono::milliseconds(1));
    for (int n = 0; n < 500 && !stop;) {
      if (roost_request(client, 42, 43, nullptr, 0))
        ++n;
      std::this_thread::sleep_for(std::chrono::milliseconds(2));
    }
  });
  unsigned replies = 0;
  bool failed = false;
  auto deadline = std::chrono::steady_clock::now() + std::chrono::seconds(10);
  while (replies < 500 && !failed &&
         std::chrono::steady_clock::now() < deadline) {
    roost_event event{};
    while (roost_poll(client, &event)) {
      if (event.type == ROOST_READY)
        ready = true;
      if (event.type == ROOST_RESPONSE && event.message_id == 43)
        ++replies;
      if (event.type == ROOST_CLOSED || event.type == ROOST_REQUEST_FAILED)
        failed = true;
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(1));
  }
  stop = true;
  application.join();
  roost_destroy(client);
  if (failed || replies != 500) {
    std::cerr << "sequence regression replies=" << replies << "\n";
    return 1;
  }
  std::cout << "PASS 500 concurrent requests with 1ms heartbeats\n";
}
