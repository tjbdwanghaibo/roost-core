#pragma once
#include <climits>
#include <google/protobuf/message.h>
#include <memory>
#include <stdexcept>
#include <unordered_map>
#include <unordered_set>
#include <vector>

namespace roost {
namespace detail {
using pb_message = google::protobuf::Message;
using pb_field = google::protobuf::FieldDescriptor;

// Generated map keys have one scalar type. The same conversion is used for
// baseline entries, upserts and the repeated removed-key field.
inline std::string dao_map_key(const pb_message &value, const pb_field *field,
                               int index = -1) {
  const auto *r = value.GetReflection();
  switch (field->cpp_type()) {
  case pb_field::CPPTYPE_INT32:
    return std::to_string(index < 0 ? r->GetInt32(value, field)
                                    : r->GetRepeatedInt32(value, field, index));
  case pb_field::CPPTYPE_INT64:
    return std::to_string(index < 0 ? r->GetInt64(value, field)
                                    : r->GetRepeatedInt64(value, field, index));
  case pb_field::CPPTYPE_UINT32:
    return std::to_string(index < 0
                              ? r->GetUInt32(value, field)
                              : r->GetRepeatedUInt32(value, field, index));
  case pb_field::CPPTYPE_UINT64:
    return std::to_string(index < 0
                              ? r->GetUInt64(value, field)
                              : r->GetRepeatedUInt64(value, field, index));
  case pb_field::CPPTYPE_BOOL:
    return (index < 0 ? r->GetBool(value, field)
                      : r->GetRepeatedBool(value, field, index))
               ? "1"
               : "0";
  case pb_field::CPPTYPE_STRING:
    return index < 0 ? r->GetString(value, field)
                     : r->GetRepeatedString(value, field, index);
  default:
    throw std::invalid_argument("unsupported DAO map key");
  }
}

// Copy one typed PB value, including map entries whose generated entry messages
// have different descriptors but identical key/value types.
inline void copy_value(const pb_message &source, const pb_field *from,
                       int index, pb_message &target, const pb_field *to) {
  if (from->cpp_type() != to->cpp_type())
    throw std::invalid_argument("DAO field type mismatch");
  const auto *read = source.GetReflection();
  const auto *write = target.GetReflection();
  switch (from->cpp_type()) {
  case pb_field::CPPTYPE_INT32: {
    auto value = index < 0 ? read->GetInt32(source, from)
                           : read->GetRepeatedInt32(source, from, index);
    if (to->is_repeated())
      write->AddInt32(&target, to, value);
    else
      write->SetInt32(&target, to, value);
    break;
  }
  case pb_field::CPPTYPE_INT64: {
    auto value = index < 0 ? read->GetInt64(source, from)
                           : read->GetRepeatedInt64(source, from, index);
    if (to->is_repeated())
      write->AddInt64(&target, to, value);
    else
      write->SetInt64(&target, to, value);
    break;
  }
  case pb_field::CPPTYPE_UINT32: {
    auto value = index < 0 ? read->GetUInt32(source, from)
                           : read->GetRepeatedUInt32(source, from, index);
    if (to->is_repeated())
      write->AddUInt32(&target, to, value);
    else
      write->SetUInt32(&target, to, value);
    break;
  }
  case pb_field::CPPTYPE_UINT64: {
    auto value = index < 0 ? read->GetUInt64(source, from)
                           : read->GetRepeatedUInt64(source, from, index);
    if (to->is_repeated())
      write->AddUInt64(&target, to, value);
    else
      write->SetUInt64(&target, to, value);
    break;
  }
  case pb_field::CPPTYPE_FLOAT: {
    auto value = index < 0 ? read->GetFloat(source, from)
                           : read->GetRepeatedFloat(source, from, index);
    if (to->is_repeated())
      write->AddFloat(&target, to, value);
    else
      write->SetFloat(&target, to, value);
    break;
  }
  case pb_field::CPPTYPE_DOUBLE: {
    auto value = index < 0 ? read->GetDouble(source, from)
                           : read->GetRepeatedDouble(source, from, index);
    if (to->is_repeated())
      write->AddDouble(&target, to, value);
    else
      write->SetDouble(&target, to, value);
    break;
  }
  case pb_field::CPPTYPE_BOOL: {
    auto value = index < 0 ? read->GetBool(source, from)
                           : read->GetRepeatedBool(source, from, index);
    if (to->is_repeated())
      write->AddBool(&target, to, value);
    else
      write->SetBool(&target, to, value);
    break;
  }
  case pb_field::CPPTYPE_ENUM: {
    auto value = index < 0 ? read->GetEnumValue(source, from)
                           : read->GetRepeatedEnumValue(source, from, index);
    if (to->is_repeated())
      write->AddEnumValue(&target, to, value);
    else
      write->SetEnumValue(&target, to, value);
    break;
  }
  case pb_field::CPPTYPE_STRING: {
    auto value = index < 0 ? read->GetString(source, from)
                           : read->GetRepeatedString(source, from, index);
    if (to->is_repeated())
      write->AddString(&target, to, value);
    else
      write->SetString(&target, to, value);
    break;
  }
  case pb_field::CPPTYPE_MESSAGE: {
    const auto &value = index < 0
                            ? read->GetMessage(source, from)
                            : read->GetRepeatedMessage(source, from, index);
    auto *out = to->is_repeated() ? write->AddMessage(&target, to)
                                  : write->MutableMessage(&target, to);
    if (value.GetDescriptor() == out->GetDescriptor()) {
      out->CopyFrom(value);
      break;
    }
    if (!from->is_map() || !to->is_map())
      throw std::invalid_argument("DAO message type mismatch");
    for (int number : {1, 2})
      copy_value(value, value.GetDescriptor()->FindFieldByNumber(number), -1,
                 *out, out->GetDescriptor()->FindFieldByNumber(number));
    break;
  }
  }
}
inline void copy_field(const pb_message &source, const pb_field *from,
                       pb_message &target, const pb_field *to) {
  target.GetReflection()->ClearField(&target, to);
  if (from->is_repeated() != to->is_repeated())
    throw std::invalid_argument("DAO field cardinality mismatch");
  if (from->is_repeated()) {
    for (int i = 0; i < source.GetReflection()->FieldSize(source, from); ++i)
      copy_value(source, from, i, target, to);
  } else
    copy_value(source, from, -1, target, to);
}
inline const pb_field *required_field(const pb_message &value, int number) {
  auto *field = value.GetDescriptor()->FindFieldByNumber(number);
  if (!field)
    throw std::invalid_argument("DAO patch schema mismatch");
  return field;
}

inline void apply_nested_patch(const pb_message &patch, pb_message &state);
inline void apply_wrapper(const pb_message &patch, const pb_message &old,
                          bool exists, pb_message &next);

// A pointer uses SyncOptionalX; a value struct uses SyncX directly.
inline void apply_nested_value(const pb_message &patch, pb_message &value) {
  const auto &name = patch.GetDescriptor()->name();
  if (name.compare(0, 9, "SyncPatch") != 0)
    throw std::invalid_argument("DAO nested patch type");
  auto expected = std::string("Sync") + std::string(name.substr(9));
  if (value.GetDescriptor()->name() == expected) {
    apply_nested_patch(patch, value);
    return;
  }
  auto *inner = required_field(value, 1);
  if (inner->is_repeated() || inner->cpp_type() != pb_field::CPPTYPE_MESSAGE ||
      !value.GetReflection()->HasField(value, inner) ||
      inner->message_type()->name() != expected)
    throw std::invalid_argument("nil or mismatched nested baseline");
  apply_nested_patch(patch,
                     *value.GetReflection()->MutableMessage(&value, inner));
}
inline void apply_nested_patch(const pb_message &patch, pb_message &state) {
  std::vector<const pb_field *> fields;
  patch.GetReflection()->ListFields(patch, &fields);
  for (auto *field : fields) {
    auto *target = required_field(state, field->number());
    if (field->is_repeated() || field->cpp_type() != pb_field::CPPTYPE_MESSAGE)
      throw std::invalid_argument("DAO patch wrapper required");
    const auto &wrapped = patch.GetReflection()->GetMessage(patch, field);
    std::unique_ptr<pb_message> baseline(wrapped.New()), result(wrapped.New());
    auto *value = required_field(wrapped, 1);
    copy_field(state, target, *baseline, value);
    apply_wrapper(wrapped, *baseline, true, *result);
    copy_field(*result, value, state, target);
  }
}

inline void apply_map(const pb_message &patch, const pb_message &old,
                      pb_message &next) {
  auto *value = required_field(patch, 1), *removed = required_field(patch, 3);
  auto *key = value->message_type()->FindFieldByNumber(1);
  const auto *r = patch.GetReflection();
  const auto *baseline = old.GetReflection();
  std::unordered_set<std::string> replaced;
  for (int i = 0; i < r->FieldSize(patch, removed); ++i)
    replaced.insert(dao_map_key(patch, removed, i));
  for (int i = 0; i < r->FieldSize(patch, value); ++i)
    replaced.insert(dao_map_key(r->GetRepeatedMessage(patch, value, i), key));
  for (int i = 0; i < baseline->FieldSize(old, value); ++i)
    if (!replaced.count(
            dao_map_key(baseline->GetRepeatedMessage(old, value, i), key)))
      copy_value(old, value, i, next, value);
  for (int i = 0; i < r->FieldSize(patch, value); ++i)
    copy_value(patch, value, i, next, value);
  auto *patches = patch.GetDescriptor()->FindFieldByNumber(4);
  if (!patches)
    return;
  std::unordered_map<std::string, int> positions;
  const auto *out = next.GetReflection();
  for (int i = 0; i < out->FieldSize(next, value); ++i)
    positions[dao_map_key(out->GetRepeatedMessage(next, value, i), key)] = i;
  for (int i = 0; i < r->FieldSize(patch, patches); ++i) {
    const auto &entry = r->GetRepeatedMessage(patch, patches, i);
    auto found = positions.find(dao_map_key(entry, required_field(entry, 1)));
    if (found == positions.end())
      throw std::invalid_argument("missing map element baseline");
    auto *destination =
        out->MutableRepeatedMessage(&next, value, found->second);
    auto *payload = required_field(*destination, 2);
    if (payload->cpp_type() != pb_field::CPPTYPE_MESSAGE)
      throw std::invalid_argument("nested map value required");
    apply_nested_value(
        entry.GetReflection()->GetMessage(entry, required_field(entry, 2)),
        *destination->GetReflection()->MutableMessage(destination, payload));
  }
}

inline void apply_list(const pb_message &patch, const pb_message &old,
                       pb_message &next) {
  const auto *r = patch.GetReflection();
  auto *value = required_field(patch, 1), *updates = required_field(patch, 3),
       *length = required_field(patch, 4);
  auto count = r->GetUInt64(patch, length);
  auto old_size = old.GetReflection()->FieldSize(old, value);
  if (count > static_cast<uint64_t>(old_size) +
                  static_cast<uint64_t>(r->FieldSize(patch, updates)) ||
      count > static_cast<uint64_t>(INT_MAX))
    throw std::invalid_argument("invalid list length");
  std::unordered_map<uint64_t, const pb_message *> indexed;
  for (int i = 0; i < r->FieldSize(patch, updates); ++i) {
    const auto &entry = r->GetRepeatedMessage(patch, updates, i);
    auto index =
        entry.GetReflection()->GetUInt64(entry, required_field(entry, 1));
    if (index >= count)
      throw std::invalid_argument("list index out of bounds");
    indexed[index] = &entry;
  }
  for (uint64_t i = 0; i < count; ++i) {
    auto found = indexed.find(i);
    if (found != indexed.end())
      copy_value(*found->second, required_field(*found->second, 2), -1, next,
                 value);
    else if (i < static_cast<uint64_t>(old_size))
      copy_value(old, value, static_cast<int>(i), next, value);
    else
      throw std::invalid_argument("list append has a hole");
  }
  auto *patches = patch.GetDescriptor()->FindFieldByNumber(5);
  if (!patches)
    return;
  for (int i = 0; i < r->FieldSize(patch, patches); ++i) {
    const auto &entry = r->GetRepeatedMessage(patch, patches, i);
    auto index =
        entry.GetReflection()->GetUInt64(entry, required_field(entry, 1));
    if (index >= count || index >= static_cast<uint64_t>(old_size) ||
        indexed.count(index))
      throw std::invalid_argument("invalid nested list operation");
    if (value->cpp_type() != pb_field::CPPTYPE_MESSAGE)
      throw std::invalid_argument("nested list value required");
    apply_nested_value(
        entry.GetReflection()->GetMessage(entry, required_field(entry, 2)),
        *next.GetReflection()->MutableRepeatedMessage(&next, value,
                                                      static_cast<int>(index)));
  }
}

inline void apply_wrapper(const pb_message &patch, const pb_message &old,
                          bool exists, pb_message &next) {
  const auto *r = patch.GetReflection();
  auto *operation = patch.GetDescriptor()->FindFieldByNumber(2);
  auto *value = required_field(patch, 1);
  bool active =
      operation && ((operation->cpp_type() == pb_field::CPPTYPE_BOOL &&
                     r->GetBool(patch, operation)) ||
                    (operation->cpp_type() == pb_field::CPPTYPE_MESSAGE &&
                     r->HasField(patch, operation)));
  if (!active) {
    std::vector<const pb_field *> fields;
    r->ListFields(patch, &fields);
    for (auto *field : fields)
      if (field->number() != 1)
        throw std::invalid_argument("operations on replacement");
    next.CopyFrom(patch);
    return;
  }
  if (!exists)
    throw std::invalid_argument("missing DAO baseline");
  next.Clear();
  if (operation->cpp_type() == pb_field::CPPTYPE_MESSAGE) {
    next.CopyFrom(old);
    apply_nested_value(r->GetMessage(patch, operation),
                       *next.GetReflection()->MutableMessage(&next, value));
  } else if (value->is_map())
    apply_map(patch, old, next);
  else if (value->is_repeated())
    apply_list(patch, old, next);
  else
    throw std::invalid_argument("invalid collection patch");
}
} // namespace detail

// Format 3 uses setter-driven typed nested patches. Publish only after all
// fields and collection indices have been validated and applied to staging.
inline void apply_dao_patch(const google::protobuf::Message &patch,
                            google::protobuf::Message &state, bool full) {
  using detail::pb_field;
  if (patch.GetDescriptor() != state.GetDescriptor())
    throw std::invalid_argument("DAO protobuf type mismatch");
  auto *id = detail::required_field(patch, 1),
       *format = detail::required_field(patch, 2047);
  const auto *r = patch.GetReflection();
  if (id->cpp_type() != pb_field::CPPTYPE_INT64 || id->is_repeated() ||
      format->cpp_type() != pb_field::CPPTYPE_UINT32 || format->is_repeated())
    throw std::invalid_argument("DAO metadata schema mismatch");
  if (r->GetUInt32(patch, format) != 3 ||
      (!full && state.GetReflection()->GetUInt32(state, format) != 3))
    throw std::invalid_argument("unsupported DAO sync format");
  // A delta may leave the entity id out (0) and keep the baseline's; when it
  // carries one, it must match.
  auto patch_id = r->GetInt64(patch, id);
  auto state_id = state.GetReflection()->GetInt64(state, id);
  if (!full && patch_id != 0 && patch_id != state_id)
    throw std::invalid_argument("DAO identity mismatch");
  std::unique_ptr<google::protobuf::Message> next(state.New());
  if (!full)
    next->CopyFrom(state);
  const auto *out = next->GetReflection();
  out->SetInt64(next.get(), id, full ? patch_id : state_id);
  out->SetUInt32(next.get(), format, 3);
  std::vector<const pb_field *> fields;
  r->ListFields(patch, &fields);
  for (auto *field : fields) {
    if (field == id || field == format)
      continue;
    if (field->is_repeated() || field->cpp_type() != pb_field::CPPTYPE_MESSAGE)
      throw std::invalid_argument("DAO presence wrapper required");
    detail::apply_wrapper(r->GetMessage(patch, field),
                          state.GetReflection()->GetMessage(state, field),
                          !full &&
                              state.GetReflection()->HasField(state, field),
                          *out->MutableMessage(next.get(), field));
  }
  state.CopyFrom(*next);
}
} // namespace roost
