#include "dao_sync.pb.h"
#include "protocol.pb.h"
#include "roost/client.hpp"
#include "roost/entity_views.hpp"
#include "roost/protobuf.hpp"
#include <chrono>
#include <cstdlib>
#include <google/protobuf/util/message_differencer.h>
#include <iostream>
#include <thread>

int main(int argc, char **argv) {
  try {
    if (argc != 3)
      return 2;
    uint16_t port = uint16_t(std::stoi(argv[1]));
    uint32_t add_exp = uint32_t(std::stoul(argv[2]));
    const char *configured_ticket = std::getenv("ROOST_GATE_TICKET");
    const std::string ticket = configured_ticket ? configured_ticket : "ticket";
    roost::client connection;
    roost::entity_views receiver;
    receiver.register_dao<roost::dao::sync::SyncPlayerDao>("player", "default");
    roost::dao::sync::SyncPlayerDao state;
    int full = 0, delta = 0, removed = 0;
    bool ready = false, closed = false, response = false;
    std::string failure;
    connection.on_callback_error = [&] {
      failure = "consumer callback failed";
    };
    connection.on_event = [&](const roost::client_event &e) {
      if (e.type == ROOST_READY) {
        ready = true;
        return;
      }
      if (e.type == ROOST_CLOSED) {
        closed = true;
        ready = false;
        receiver.reset();
        return;
      }
      if (e.type != ROOST_PUSH || e.kind != 1)
        return;
      std::vector<roost::protobuf_change> changes;
      if (!receiver.receive(e.payload.data(), e.payload.size(), changes))
        return;
      for (const auto &change : changes) {
        if (change.wire.kind == roost::change_kind::remove) {
          state.Clear();
          ++removed;
          continue;
        }
        const auto *value =
            dynamic_cast<const roost::dao::sync::SyncPlayerDao *>(
                change.current.get());
        if (!value)
          throw std::runtime_error("unexpected DAO type");
        state = *value;
        if (change.wire.kind == roost::change_kind::full)
          ++full;
        else
          ++delta;
      }
    };
    auto wait = [&](const std::function<bool()> &done) {
      auto deadline =
          std::chrono::steady_clock::now() + std::chrono::seconds(10);
      while (!done()) {
        connection.pump();
        if (!failure.empty())
          throw std::runtime_error(failure);
        if (std::chrono::steady_clock::now() > deadline)
          throw std::runtime_error("Gate client deadline");
        std::this_thread::sleep_for(std::chrono::milliseconds(1));
      }
    };
    connection.connect("127.0.0.1", port, "invalid-ticket");
    wait([&] { return closed; });
    if (ready)
      throw std::runtime_error("invalid ticket accepted");
    closed = false;
    connection.connect("127.0.0.1", port, ticket);
    wait([&] { return full == 1; });
    auto level = state.profile().value().level();
    roost::protocol::AddExpRequest request;
    request.set_amount(250);
    connection.request_pb(
        add_exp, add_exp, request, [&](const roost::client_event &e) {
          roost::protocol::AddExpResponse result;
          if (e.type != ROOST_RESPONSE ||
              !result.ParseFromArray(e.payload.data(), int(e.payload.size())) ||
              result.code() != 0 || result.levels_gained() <= 0)
            throw std::runtime_error("AddExp failed");
          response = true;
        });
    wait([&] { return response && delta > 0; });
    if (state.profile().value().level() <= level)
      throw std::runtime_error("incremental level missing");
    auto committed = state;
    std::cout << "READY_REMOVE" << std::endl;
    wait([&] { return removed == 1; });
    connection.disconnect();
    connection.pump();
    connection.connect("127.0.0.1", port, ticket);
    wait([&] { return full == 2; });
    if (!google::protobuf::util::MessageDifferencer::Equals(state, committed))
      throw std::runtime_error("reconnect state mismatch");
    std::cout << "PASS C++ Gate/PB/Nest/DAO/Sync full/delta/remove/reconnect"
              << std::endl;
  } catch (const std::exception &e) {
    std::cerr << e.what() << "\n";
    return 1;
  }
}
