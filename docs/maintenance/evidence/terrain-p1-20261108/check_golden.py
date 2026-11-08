# 独立实现（只按 README / pack.go 头注释的格式与公式），核对 Go 生成的金样。
import sys, json, struct, zlib, hashlib, os
spec = sys.argv[1]
g = json.load(open(os.path.join(spec, 'terrain-queries.json')))
data = open(os.path.join(spec, g['pack']), 'rb').read()
assert hashlib.sha256(data).hexdigest() == g['pack_sha256']
assert data[:8] == b'RTERRAIN'
ver, hsize, rev, cx, cy, ns, sp, step, base, water, codec, ioff, ient = struct.unpack_from('<IIqIIIIIiiIQI', data, 8)
assert (ver, hsize, ns, sp, step, codec, ioff, ient) == (1, 128, 64, 100, 10, 1, 128, 44)
n = cx * cy
table = data[128:128 + n * 44]
assert hashlib.sha256(data[:96] + table).digest() == data[96:128]
chunks = {}
for idx in range(n):
    off, ln = struct.unpack_from('<QI', table, idx * 44)
    want = table[idx * 44 + 12: idx * 44 + 44]
    stored = data[off:off + ln]
    assert hashlib.sha256(stored).digest() == want
    plain = zlib.decompress(stored, -15)
    h = [0] * 4096; pos = 0
    for k in range(4096):
        u = 0; shift = 0
        while True:
            b = plain[pos]; pos += 1
            u |= (b & 0x7f) << shift; shift += 7
            if b < 0x80: break
        d = (u >> 1) ^ -(u & 1)
        i, j = k % 64, k // 64
        if i == 0 and j == 0: p = 0
        elif j == 0: p = h[k-1]
        elif i == 0: p = h[k-64]
        else: p = h[k-1] + h[k-64] - h[k-65]
        h[k] = p + d
        assert 0 <= h[k] <= 65535
    m = plain[pos:]
    assert len(m) == 4096
    chunks[(idx % cx, idx // cx)] = (h, m)
for c in g['chunks']:
    h, m = chunks[(c['x'], c['y'])]
    assert hashlib.sha256(struct.pack('<4096H', *h)).hexdigest() == c['heights_sha256']
    assert hashlib.sha256(bytes(m)).hexdigest() == c['materials_sha256']
W, H = cx * 6400, cy * 6400
TX, TY = cx * 64, cy * 64
def raw(gx, gy):
    h, _ = chunks[(gx // 64, gy // 64)]
    return h[(gy % 64) * 64 + gx % 64]
bad = 0
for q in g['queries']:
    x, y = q['x'], q['y']
    if x < 0 or y < 0 or x >= W or y >= H:
        ok = q.get('error') == 'out_of_map'
    else:
        gx, gy, fx, fy = x // 100, y // 100, x % 100, y % 100
        gx1 = gx + 1 if fx > 0 and gx + 1 < TX else gx
        gy1 = gy + 1 if fy > 0 and gy + 1 < TY else gy
        num = (raw(gx, gy)*(100-fx) + raw(gx1, gy)*fx)*(100-fy) + (raw(gx, gy1)*(100-fx) + raw(gx1, gy1)*fx)*fy
        height = base + (num*10 + 5000) // 10000
        mat = chunks[(gx // 64, gy // 64)][1][(gy % 64) * 64 + gx % 64]
        ok = (q['height'], q['material'], q['water']) == (height, mat, height < water)
    if not ok:
        bad += 1; print('mismatch', q)
print('queries', len(g['queries']), 'mismatches', bad)
sys.exit(1 if bad else 0)
