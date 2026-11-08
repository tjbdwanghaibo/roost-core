using System;

namespace Roost.Client.Terrain
{
    // 地形 SDK 的错误基类。子类与 Go gameplay/terrain 的哨兵错误一一对应，调用方按类型区分处理方式；
    // 没有任何错误会用默认数据代替（不猜测缺失或损坏的地形）。
    public class TerrainException : Exception
    {
        public TerrainException(string message) : base(message) { }
        public TerrainException(string message, Exception inner) : base(message, inner) { }
    }

    // 包头或索引表不合法（magic、尺寸、校验和、越界偏移、截断），整个包不可用。对应 Go ErrBadPack。
    public sealed class TerrainBadPackException : TerrainException
    {
        public TerrainBadPackException(string message) : base("terrain: bad base pack: " + message) { }
    }

    // 包的格式版本或几何常量与本 SDK 不一致：需要用匹配的工具重新生成，不尝试兼容。对应 Go ErrPackVersion。
    public sealed class TerrainPackVersionException : TerrainException
    {
        public TerrainPackVersionException(string message) : base("terrain: unsupported base pack version: " + message) { }
    }

    // 单块数据损坏（SHA-256 不符、截断、解压或解码失败）。只影响这一块，其他块照常可读。对应 Go ErrCorruptChunk。
    public sealed class TerrainCorruptChunkException : TerrainException
    {
        public ChunkKey Key { get; }
        public TerrainCorruptChunkException(ChunkKey key, string message, Exception? inner = null)
            : base("terrain: corrupt chunk " + key + ": " + message, inner ?? new Exception(message)) { Key = key; }
    }

    // 坐标或块不在地图范围内。对应 Go ErrOutOfMap。
    public sealed class TerrainOutOfMapException : TerrainException
    {
        public TerrainOutOfMapException(string message) : base("terrain: outside the map: " + message) { }
    }

    // 查询需要的块没装入（或已卸下）；Key 是缺的那一块。调用方先装块再查。对应 Go ErrNotLoaded。
    public sealed class TerrainNotLoadedException : TerrainException
    {
        public ChunkKey Key { get; }
        public TerrainNotLoadedException(ChunkKey key) : base("terrain: chunk not loaded: " + key) { Key = key; }
    }

    // 块数据形状不对（长度不是 4096、高度越出 uint16）或覆盖块 Version < 1。对应 Go ErrInvalidChunk。
    public sealed class TerrainInvalidChunkException : TerrainException
    {
        public TerrainInvalidChunkException(string message) : base("terrain: invalid chunk data: " + message) { }
    }

    // 覆盖块的版本比视图里已有的覆盖块旧，没有装入。对应 Go ErrStaleOverlay。
    public sealed class TerrainStaleOverlayException : TerrainException
    {
        public TerrainStaleOverlayException(string message) : base("terrain: stale overlay version: " + message) { }
    }

    // 覆盖块基于另一版原始地形（BaseRevision 与包头不同），没有装入。客户端应当更新原始地形资源。对应 Go ErrBaseRevision。
    public sealed class TerrainBaseRevisionException : TerrainException
    {
        public TerrainBaseRevisionException(string message) : base("terrain: overlay base revision differs from the pack: " + message) { }
    }
}
