#include "roost/client.hpp"
#include <chrono>
#include <iostream>
#include <thread>

// 两个客户端使用同一业务 SessionID。Go 夹具控制旧 Bind 的完成时机，
// 不用 sleep 猜连接先后；真实 TCP / NATS 链路决定旧握手的最终结果。
int main(int argc, char **argv) {
  try {
    if (argc != 3)
      return 2;
    roost::client old, newer;
    bool old_ready = false, old_closed = false, new_ready = false;
    std::string failure;
    old.on_callback_error = newer.on_callback_error = [&] {
      failure = "native client callback failed";
    };
    old.on_event = [&](const roost::client_event &event) {
      if (event.type == ROOST_READY)
        old_ready = true;
      if (event.type == ROOST_CLOSED)
        old_closed = true;
    };
    newer.on_event = [&](const roost::client_event &event) {
      if (event.type == ROOST_READY)
        new_ready = true;
      if (event.type == ROOST_CLOSED)
        failure = "newer connection was closed";
    };
    auto wait = [&](const std::function<bool()> &done) {
      auto deadline = std::chrono::steady_clock::now() +
                      std::chrono::seconds(10);
      while (!done()) {
        old.pump();
        newer.pump();
        if (!failure.empty())
          throw std::runtime_error(failure);
        if (old_ready)
          throw std::runtime_error("late old handshake was acknowledged");
        if (std::chrono::steady_clock::now() >= deadline)
          throw std::runtime_error("cross-Gate deadline");
        std::this_thread::sleep_for(std::chrono::milliseconds(1));
      }
    };
    auto command = [&](const char *expected) {
      std::string line;
      if (!std::getline(std::cin, line) || line != expected)
        throw std::runtime_error("unexpected fixture command");
    };
    auto echo = [&](const std::string &text) {
      std::vector<uint8_t> payload(text.begin(), text.end());
      bool replied = false;
      newer.request(101, 101, payload,
                    [&](const roost::client_event &event) {
                      if (event.type != ROOST_RESPONSE ||
                          event.payload != payload)
                        failure = "newer connection echo failed";
                      replied = true;
                    },
                    2000);
      wait([&] { return replied; });
    };

    old.connect("127.0.0.1", uint16_t(std::stoul(argv[1])), "old");
    command("connect-new"); // Go 已确认旧 Bind 进入鉴权，随后才创建新连接。
    newer.connect("127.0.0.1", uint16_t(std::stoul(argv[2])), "new");
    wait([&] { return new_ready; });
    echo("before-late-bind");
    std::cout << "new-ready" << std::endl;
    command("old-released");
    wait([&] { return old_closed; });
    echo("after-late-bind");
    std::cout << "PASS C++ same-session cross-Gate late-bind" << std::endl;
    return 0;
  } catch (const std::exception &error) {
    std::cerr << error.what() << '\n';
    return 1;
  }
}
