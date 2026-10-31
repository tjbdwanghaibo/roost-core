#pragma once
#include <google/protobuf/message.h>
#include <memory>
#include <stdexcept>
#include <vector>
namespace roost {
// Generated DAO fields use presence wrappers. Replace present fields; MergeFrom
// would append collections.
inline void apply_dao_patch(const google::protobuf::Message &patch,
                            google::protobuf::Message &state, bool full) {
  if (patch.GetDescriptor() != state.GetDescriptor())
    throw std::invalid_argument("DAO protobuf type mismatch");
  const auto *id_field = patch.GetDescriptor()->FindFieldByNumber(1);
  if (!id_field ||
      id_field->cpp_type() !=
          google::protobuf::FieldDescriptor::CPPTYPE_INT64 ||
      id_field->is_repeated())
    throw std::invalid_argument("DAO entity_id required");
  if (!full && patch.GetReflection()->GetInt64(patch, id_field) !=
                   state.GetReflection()->GetInt64(state, id_field))
    throw std::invalid_argument("DAO identity mismatch");
  std::unique_ptr<google::protobuf::Message> next(state.New());
  if (!full)
    next->CopyFrom(state);
  auto &target = *next;
  const auto *from = patch.GetReflection();
  const auto *to = target.GetReflection();
  std::vector<const google::protobuf::FieldDescriptor *> fields;
  from->ListFields(patch, &fields);
  for (const auto *field : fields) {
    if (field->number() == 1) {
      if (field->cpp_type() != google::protobuf::FieldDescriptor::CPPTYPE_INT64)
        throw std::invalid_argument("DAO entity_id type");
      auto id = from->GetInt64(patch, field);
      auto old = to->GetInt64(target, field);
      if (!full && old && old != id)
        throw std::invalid_argument("DAO identity mismatch");
      to->SetInt64(&target, field, id);
    } else {
      if (field->is_repeated() ||
          field->cpp_type() !=
              google::protobuf::FieldDescriptor::CPPTYPE_MESSAGE)
        throw std::invalid_argument("DAO presence wrapper required");
      to->MutableMessage(&target, field)
          ->CopyFrom(from->GetMessage(patch, field));
    }
  }
  state.CopyFrom(target);
}
} // namespace roost
