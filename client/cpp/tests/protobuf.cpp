#include "roost/protobuf.hpp"
#include "dao_sync.pb.h"
#include <fstream>
#include <google/protobuf/util/message_differencer.h>
#include <iostream>
#include <iterator>
static std::string read(const char *file) {
  std::ifstream in(file, std::ios::binary);
  return {std::istreambuf_iterator<char>(in), {}};
}
int main(int argc, char **argv) {
  try {
    if (argc != 3 && argc != 4)
      return 2;
    roost::dao::sync::SyncHeroDao full, patch, state;
    if (!full.ParseFromString(read(argv[1])) ||
        !patch.ParseFromString(read(argv[2])))
      throw std::runtime_error("decode failed");
    roost::apply_dao_patch(full, state, true);
    if (argc == 4) {
      roost::dao::sync::SyncHeroDao expected;
      if (!expected.ParseFromString(read(argv[3])))
        throw std::runtime_error("expected decode failed");
      roost::apply_dao_patch(patch, state, false);
      if (!google::protobuf::util::MessageDifferencer::Equals(state, expected))
        throw std::runtime_error("collection apply mismatch");
      auto before = state;
      patch.Clear();
      patch.set_entity_id(state.entity_id());
      patch.set_sync_format(3);
      patch.mutable_name()->set_value("must-not-apply");
      auto *list = patch.mutable_friends();
      list->set_delta(true);
      list->set_length(state.friends().value_size() + 1);

      try {
        roost::apply_dao_patch(patch, state, false);
        throw std::logic_error("invalid list growth accepted");
      } catch (const std::invalid_argument &) {
      }
      if (!google::protobuf::util::MessageDifferencer::Equals(state, before))
        throw std::runtime_error("invalid collection patch partly applied");
      std::cout
          << "PASS native collection delta/full fallback/atomic rejection\n";
      return 0;
    }

    if (state.entity_id() != 9007199254740993LL || state.level().value() != 7 ||
        state.exp().value() != (int64_t(1) << 60))
      throw std::runtime_error("full/precision failed");
    if (!state.equips().value().count(4) ||
        state.equips().value().at(4).has_value())
      throw std::runtime_error("nil map element lost");
    auto before = state;
    roost::apply_dao_patch(patch, state, false);
    if (state.level().value() != 0 || !state.name().value().empty() ||
        !state.items().value().empty() || !state.friends().value().empty() ||
        state.exp().value() != before.exp().value())
      throw std::runtime_error("zero/empty/delta failed");
    auto expected = state.SerializeAsString();
    patch.set_entity_id(1);
    try {
      roost::apply_dao_patch(patch, state, false);
      throw std::logic_error("identity mismatch accepted");
    } catch (const std::invalid_argument &) {
    }
    if (state.SerializeAsString() != expected)
      throw std::runtime_error("failed patch partly applied");
    std::cout
        << "PASS native generated DAO PB "
           "full/delta/precision/zero/empty/nil/atomic identity rejection\n";
  } catch (const std::exception &e) {
    std::cerr << e.what() << "\n";
    return 1;
  }
}
