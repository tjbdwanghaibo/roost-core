#include "roost/views.h"
#include "dao_sync.pb.h"
#include <fstream>
#include <google/protobuf/descriptor.pb.h>
#include <iostream>
#include <iterator>
#include <set>
using bytes = std::vector<uint8_t>;
static void check(bool ok, const char *why) {
  if (!ok)
    throw std::runtime_error(why);
}
static void descriptors(const google::protobuf::FileDescriptor *file,
                        google::protobuf::FileDescriptorSet &set,
                        std::set<std::string> &seen) {
  if (!seen.insert(std::string(file->name())).second)
    return;
  for (int i = 0; i < file->dependency_count(); ++i)
    descriptors(file->dependency(i), set, seen);
  file->CopyTo(set.add_file());
}
int main(int argc, char **argv) {
  try {
    check(argc == 4, "full/delta/remove fixture required");
    google::protobuf::FileDescriptorSet set;
    std::set<std::string> seen;
    descriptors(roost::dao::sync::SyncHeroDao::descriptor()->file(), set, seen);
    auto encoded = set.SerializeAsString();
    auto *views = roost_views_create(
        reinterpret_cast<const uint8_t *>(encoded.data()), encoded.size());
    check(views, "views create");
    struct cleanup {
      roost_views *p;
      ~cleanup() { roost_views_destroy(p); }
    } guard{views};
    check(roost_views_register(views, "roost.dao.sync.SyncHeroDao", "hero",
                               "default", 1),
          "register schema");
    check(!roost_views_register(views, "Missing", "hero", "default", 2),
          "unknown schema refused");
    for (int i = 1; i <= 3; ++i) {
      std::ifstream file(argv[i], std::ios::binary);
      bytes packet{std::istreambuf_iterator<char>(file), {}};
      const roost_view_change *changes = nullptr;
      size_t count = 0;
      check(roost_views_receive(views, packet.data(), packet.size(), &changes,
                                &count),
            roost_views_error(views));
      check(count == 1 && changes[0].operation == uint32_t(i - 1) &&
                changes[0].entity_id == 9007199254740993LL,
            "change metadata");
      if (i < 3) {
        roost::dao::sync::SyncHeroDao current;
        check(current.ParseFromArray(changes[0].data, int(changes[0].size)),
              "merged PB decode");
        check(current.level().value() == (i == 1 ? 7 : 0) &&
                  current.exp().value() == (int64_t(1) << 60),
              "merged PB state");
      } else
        check(changes[0].size == 0, "remove empty");
      check(roost_views_receive(views, packet.data(), packet.size(), &changes,
                                &count) &&
                count == 0,
            "duplicate has no callback");
    }
    const roost_view_change *out = nullptr;
    size_t count = 0;
    check(!roost_views_receive(views, nullptr, 1, &out, &count) && !out &&
              count == 0,
          "bad packet clears output");
    roost_views_reset(views);
    std::cout << "PASS C ABI descriptor registry, atomic Full/Delta/Remove PB "
                 "views\n";
  } catch (const std::exception &e) {
    std::cerr << e.what() << "\n";
    return 1;
  }
}
