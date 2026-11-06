// 鉴权首包（message_id=0 的第一帧）载荷的连接代号信封，库内部使用（tests/smoke.cpp 用它核对 Go 金样）。
// 布局：0x00、'G'、8 字节大端连接代号（非 0）、票据原文（client/README“鉴权首包的连接代号”，Go 侧
// client/wire.EncodeAuth）。同一 SessionID 的两条连接落在不同 Gate 实例时，Game 按代号认出迟到完成的旧握手，
// 不让它断开更新的连接；服务端在鉴权前剥掉信封，业务鉴权只看到票据原文。
#ifndef ROOST_AUTH_PAYLOAD_HPP
#define ROOST_AUTH_PAYLOAD_HPP
#include <cstddef>
#include <cstdint>
#include <vector>
namespace roost_detail {
constexpr size_t auth_envelope_overhead = 2 + 8;
// 票据为空、以 0x00 开头（服务端会当成坏信封）或代号为 0 时返回 false，out 不变。
inline bool encode_auth(const uint8_t *ticket, size_t size, uint64_t generation,
                        std::vector<uint8_t> &out) {
  if (!ticket || !size || ticket[0] == 0 || !generation)
    return false;
  out.assign(auth_envelope_overhead + size, 0);
  out[1] = 'G';
  for (int i = 0; i < 8; ++i)
    out[2 + i] = uint8_t(generation >> (56 - 8 * i));
  for (size_t i = 0; i < size; ++i)
    out[auth_envelope_overhead + i] = ticket[i];
  return true;
}
} // namespace roost_detail
#endif
