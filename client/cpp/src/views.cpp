#include "roost/views.h"
#include "roost/entity_views.hpp"
#include <array>
#include <climits>
#include <cstdio>
#include <google/protobuf/descriptor.pb.h>
#include <google/protobuf/dynamic_message.h>
struct roost_views {
  // 顺序保证 views 中的动态消息先析构，随后才销毁 factory 和 descriptor pool。
  google::protobuf::DescriptorPool pool;
  google::protobuf::DynamicMessageFactory factory{&pool};
  roost::entity_views views;
  std::vector<std::string> payloads;
  std::vector<roost_view_change> changes;
  std::array<char, 256> error{};
  template <class F> int call(F work) noexcept {
    try {
      work();
      error[0] = 0;
      return 1;
    } catch (const std::exception &e) {
      std::snprintf(error.data(), error.size(), "%s", e.what());
    } catch (...) {
      std::snprintf(error.data(), error.size(), "%s", "unknown Sync failure");
    }
    return 0;
  }
};
extern "C" {
roost_views *roost_views_create(const uint8_t *data, size_t size) {
  if (!data || !size || size > INT_MAX)
    return nullptr;
  try {
    google::protobuf::FileDescriptorSet files;
    if (!files.ParseFromArray(data, int(size)) || !files.file_size())
      return nullptr;
    auto result = std::make_unique<roost_views>();
    // protoc 输出按依赖顺序排列；缺依赖/重复/不合法的 schema 明确拒绝。
    for (const auto &file : files.file())
      if (!result->pool.BuildFile(file))
        return nullptr;
    return result.release();
  } catch (...) {
    return nullptr;
  }
}
int roost_views_register(roost_views *v, const char *type, const char *name,
                         const char *profile, uint32_t schema) {
  if (!v)
    return 0;
  return v->call([&] {
    if (!type || !name || !profile)
      throw std::invalid_argument("DAO type/name/profile required");
    auto descriptor = v->pool.FindMessageTypeByName(type);
    if (!descriptor)
      throw std::invalid_argument("DAO descriptor not found");
    auto prototype = v->factory.GetPrototype(descriptor);
    if (!prototype)
      throw std::runtime_error("DAO prototype unavailable");
    v->views.register_codec(name, profile, schema, [prototype] {
      return std::unique_ptr<google::protobuf::Message>(prototype->New());
    });
  });
}
int roost_views_receive(roost_views *v, const uint8_t *data, size_t size,
                        const roost_view_change **out, size_t *count) {
  if (out)
    *out = nullptr;
  if (count)
    *count = 0;
  if (!v || !out || !count)
    return 0;
  int ok = v->call([&] {
    v->changes.clear();
    v->payloads.clear();
    if (!data || !size)
      throw std::invalid_argument("Sync packet required");
    std::vector<roost::protobuf_change> changes;
    v->views.receive(data, size, changes);
    v->payloads.reserve(changes.size());
    v->changes.reserve(changes.size());
    for (const auto &change : changes) {
      std::string payload;
      if (change.current && !change.current->SerializeToString(&payload))
        throw std::runtime_error("DAO serialization failed");
      v->payloads.push_back(std::move(payload));
      const auto &wire = change.wire;
      v->changes.push_back({uint32_t(wire.kind), wire.object, wire.generation,
                            wire.component, wire.update.id, wire.update.version,
                            nullptr, 0});
    }
    for (size_t i = 0; i < v->changes.size(); ++i) {
      v->changes[i].data =
          reinterpret_cast<const uint8_t *>(v->payloads[i].data());
      v->changes[i].size = v->payloads[i].size();
    }
    *out = v->changes.data();
    *count = v->changes.size();
  });
  if (!ok) {
    v->views.reset();
    v->changes.clear();
    v->payloads.clear();
  }
  return ok;
}
const char *roost_views_error(const roost_views *v) {
  return v ? v->error.data() : "null views";
}
void roost_views_reset(roost_views *v) {
  if (v) {
    v->views.reset();
    v->changes.clear();
    v->payloads.clear();
    v->error[0] = 0;
  }
}
void roost_views_destroy(roost_views *v) { delete v; }
}
