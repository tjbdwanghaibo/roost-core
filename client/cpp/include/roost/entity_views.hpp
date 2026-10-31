#pragma once
#include "protobuf.hpp"
#include "sync.hpp"
#include <functional>
#include <memory>
#include <tuple>
namespace roost {
struct protobuf_view {
  uint16_t generation = 0;
  subject metadata;
  std::shared_ptr<const google::protobuf::Message> value;
};
struct protobuf_change {
  change wire;
  std::shared_ptr<const google::protobuf::Message> previous, current;
};
// 消费线程中的 DAO
// 状态视图。解析成功前不替换任何业务对象；回调由调用方在返回后派发。
class entity_views {
  using key = std::pair<uint16_t, uint16_t>;
  using codec_key = std::tuple<std::string, std::string, uint16_t, uint32_t>;
  sync_receiver receiver_;
  std::map<key, protobuf_view> views_;
  std::map<codec_key,
           std::function<std::unique_ptr<google::protobuf::Message>()>>
      codecs_;

public:
  template <class Message>
  void register_dao(std::string name, std::string profile = "self",
                    uint32_t schema = 0) {
    register_codec(std::move(name), std::move(profile), schema, [] {
      return std::unique_ptr<google::protobuf::Message>(new Message);
    });
  }
  void register_codec(
      std::string name, std::string profile, uint32_t schema,
      std::function<std::unique_ptr<google::protobuf::Message>()> factory) {
    if (!factory || name.empty() || profile.empty())
      throw std::invalid_argument("DAO codec requires name/profile/factory");
    auto inserted = codecs_.emplace(
        codec_key{std::move(name), std::move(profile), 2, schema},
        std::move(factory));
    if (!inserted.second)
      throw std::invalid_argument("DAO codec already registered");
  }
  void reset() {
    receiver_.reset();
    views_.clear();
  }
  const std::map<key, protobuf_view> &current() const { return views_; }
  bool receive(const uint8_t *raw, size_t size,
               std::vector<protobuf_change> &changes) {
    changes.clear();
    std::vector<change> patches;
    if (!receiver_.receive(raw, size, patches))
      return false;
    try {
      auto next = views_;
      std::vector<protobuf_change> result;
      for (const auto &patch : patches) {
        key identity{patch.object, patch.component};
        auto old = next.find(identity);
        auto previous = old == next.end()
                            ? std::shared_ptr<const google::protobuf::Message>{}
                            : old->second.value;
        if (patch.kind == change_kind::remove) {
          next.erase(identity);
          result.push_back({patch, previous, {}});
          continue;
        }
        const auto &update = patch.update;
        auto factory = codecs_.find(codec_key{update.name, update.profile,
                                              update.encoding, update.schema});
        if (factory == codecs_.end())
          throw std::invalid_argument("DAO codec not registered");
        std::shared_ptr<const google::protobuf::Message> value;
        if (patch.kind == change_kind::delta && update.payload.empty())
          value = previous;
        else {
          auto parsed = factory->second();
          if (!parsed)
            throw std::runtime_error("DAO factory returned null");
          if (!parsed->ParseFromArray(update.payload.data(),
                                      int(update.payload.size())))
            throw std::invalid_argument("invalid DAO protobuf payload");
          auto state = factory->second();
          if (!state)
            throw std::runtime_error("DAO factory returned null");
          if (patch.kind == change_kind::delta) {
            if (!previous)
              throw std::invalid_argument("DAO baseline missing");
            state->CopyFrom(*previous);
          }
          apply_dao_patch(*parsed, *state, patch.kind == change_kind::full);
          value = std::shared_ptr<const google::protobuf::Message>(
              std::move(state));
        }
        auto metadata = update;
        metadata.payload.clear();
        next[identity] = {patch.generation, std::move(metadata), value};
        result.push_back({patch, previous, value});
      }
      views_ = std::move(next);
      changes = std::move(result);
      return true;
    } catch (...) {
      // 协议元数据已推进；解码失败后废弃基线，业务必须断开并重新取 Full。
      receiver_.reset();
      throw;
    }
  }
};
} // namespace roost
