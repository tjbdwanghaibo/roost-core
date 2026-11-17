using System;
using System.IO;
using System.IO.Compression;

namespace Roost.Client.Relay
{
    /// <summary>
    /// 压缩快照与交接数据（房主发出前压缩，新房主收到 YouAreHost 时解压）。relay 不压缩、不解压、不解析，房间里每个客户端
    /// 要用同一种。缺省 <see cref="RelayCompressors.Deflate"/>；游戏可以换成 LZ4 或 zstd 的实现（SDK 不带第三方依赖）。
    /// </summary>
    public interface IRelayCompressor
    {
        byte[] Compress(ReadOnlySpan<byte> data);
        /// <summary>解出的字节超过 limit 时抛 InvalidDataException（防解压炸弹）。</summary>
        byte[] Decompress(ReadOnlySpan<byte> data, int limit);
    }

    /// <summary>自带的两种压缩。</summary>
    public static class RelayCompressors
    {
        /// <summary>RFC 1951 原始 deflate（.NET DeflateStream），与 Go SDK 的 relay.Deflate（compress/flate）互通。</summary>
        public static readonly IRelayCompressor Deflate = new DeflateCompressor();
        /// <summary>原样发出、原样交出。</summary>
        public static readonly IRelayCompressor None = new NoCompressor();
        /// <summary>YouAreHost 里状态字节解压后的上限（与 Go SDK 的 relay.MaxStateBytes 相同）。</summary>
        public const int MaxStateBytes = 16 << 20;

        private sealed class DeflateCompressor : IRelayCompressor
        {
            public byte[] Compress(ReadOnlySpan<byte> data)
            {
                var output = new MemoryStream();
                using (var deflate = new DeflateStream(output, CompressionLevel.Optimal, leaveOpen: true)) deflate.Write(data);
                return output.ToArray();
            }

            public byte[] Decompress(ReadOnlySpan<byte> data, int limit)
            {
                using var deflate = new DeflateStream(new MemoryStream(data.ToArray()), CompressionMode.Decompress);
                var output = new MemoryStream();
                var buffer = new byte[16 << 10];
                int read;
                while ((read = deflate.Read(buffer, 0, buffer.Length)) > 0)
                {
                    if (output.Length + read > limit) throw new InvalidDataException($"Inflated relay state exceeds {limit} bytes.");
                    output.Write(buffer, 0, read);
                }
                return output.ToArray();
            }
        }

        private sealed class NoCompressor : IRelayCompressor
        {
            public byte[] Compress(ReadOnlySpan<byte> data) => data.ToArray();

            public byte[] Decompress(ReadOnlySpan<byte> data, int limit)
            {
                if (data.Length > limit) throw new InvalidDataException($"Relay state exceeds {limit} bytes.");
                return data.ToArray();
            }
        }
    }
}
