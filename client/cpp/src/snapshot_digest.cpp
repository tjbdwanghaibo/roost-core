#include "roost/lockstep.hpp"
#include <algorithm>
namespace roost {
// SHA-256 用于与 Go 快照摘要逐字节互验；不接收密钥，不承担认证功能。
std::array<uint8_t, 32> snapshot_digest(const bytes &data) {
  static constexpr uint32_t k[64] = {
      0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1,
      0x923f82a4, 0xab1c5ed5, 0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3,
      0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174, 0xe49b69c1, 0xefbe4786,
      0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
      0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147,
      0x06ca6351, 0x14292967, 0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13,
      0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85, 0xa2bfe8a1, 0xa81a664b,
      0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
      0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a,
      0x5b9cca4f, 0x682e6ff3, 0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208,
      0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2};
  uint32_t h[8] = {0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
                   0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19};
  auto rotate = [](uint32_t x, int n) { return (x >> n) | (x << (32 - n)); };
  const size_t blocks = data.size() / 64 + (data.size() % 64 < 56 ? 1 : 2);
  const uint64_t bits = uint64_t(data.size()) * 8;
  for (size_t block = 0; block < blocks; ++block) {
    uint8_t chunk[64]{};
    for (size_t i = 0; i < 64; ++i) {
      size_t at = block * 64 + i;
      if (at < data.size())
        chunk[i] = data[at];
      else if (at == data.size())
        chunk[i] = 0x80;
    }
    if (block + 1 == blocks)
      for (int i = 0; i < 8; ++i)
        chunk[63 - i] = uint8_t(bits >> (8 * i));
    uint32_t w[64]{};
    for (int i = 0; i < 16; ++i)
      for (int j = 0; j < 4; ++j)
        w[i] = (w[i] << 8) | chunk[i * 4 + j];
    for (int i = 16; i < 64; ++i) {
      auto a = w[i - 15], b = w[i - 2];
      w[i] = w[i - 16] + (rotate(a, 7) ^ rotate(a, 18) ^ (a >> 3)) + w[i - 7] +
             (rotate(b, 17) ^ rotate(b, 19) ^ (b >> 10));
    }
    uint32_t a = h[0], b = h[1], c = h[2], d = h[3], e = h[4], f = h[5],
             g = h[6], z = h[7];
    for (int i = 0; i < 64; ++i) {
      auto first = z + (rotate(e, 6) ^ rotate(e, 11) ^ rotate(e, 25)) +
                   ((e & f) ^ (~e & g)) + k[i] + w[i];
      auto second = (rotate(a, 2) ^ rotate(a, 13) ^ rotate(a, 22)) +
                    ((a & b) ^ (a & c) ^ (b & c));
      z = g;
      g = f;
      f = e;
      e = d + first;
      d = c;
      c = b;
      b = a;
      a = first + second;
    }
    h[0] += a;
    h[1] += b;
    h[2] += c;
    h[3] += d;
    h[4] += e;
    h[5] += f;
    h[6] += g;
    h[7] += z;
  }
  std::array<uint8_t, 32> out{};
  for (int i = 0; i < 8; ++i)
    for (int j = 0; j < 4; ++j)
      out[i * 4 + j] = uint8_t(h[i] >> (24 - j * 8));
  return out;
}
} // namespace roost
