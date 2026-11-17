using System;
using System.IO;
using System.IO.Compression;

namespace Roost.Client.Relay
{
    /// <summary>
    /// 压缩快照与交接数据（房主发出前压缩，新房主收到 YouAreHost 时解压）。relay 不压缩、不解压、不解析。
    /// 缺省 <see cref="RelayCompressors.Deflate"/>；游戏可以换成 LZ4 或 zstd 的实现（SDK 不带第三方依赖，接法见本工程 README）。
    /// <para>
    /// Id 是压缩方式标识（三份 SDK 之间的约定，docs/framework/RELAY.md §2.3）：SDK 把它作为 1 个字节写在压缩后的状态字节前面，
    /// 收到时按它选解压方式——自己的 Compressor，或内置的 None（0）、Deflate（1）。0 与 1 归内置的两种，游戏自带的用 2～255
    /// （README 的示例里 zstd 用 2、LZ4 用 3）。标识不认识时 RelayYouAreHost.StateError 是 <see cref="RelayUnknownCompressionException"/>。
    /// </para>
    /// </summary>
    public interface IRelayCompressor
    {
        /// <summary>压缩方式标识（见接口说明）。</summary>
        byte Id { get; }
        byte[] Compress(ReadOnlySpan<byte> data);
        /// <summary>解出的字节超过 limit 时抛 InvalidDataException（防解压炸弹）。</summary>
        byte[] Decompress(ReadOnlySpan<byte> data, int limit);
    }

    /// <summary>收到的状态字节的压缩方式标识不认识（既不是自己的 Compressor.Id，也不是内置的两种）：房间里有人用了别的压缩方式。</summary>
    public sealed class RelayUnknownCompressionException : Exception
    {
        /// <summary>收到的标识。</summary>
        public byte Method { get; }
        public RelayUnknownCompressionException(byte method, byte own)
            : base($"Relay state uses an unknown compression method {method} (this client compresses with {own}).") { Method = method; }
    }

    /// <summary>自带的两种压缩，以及状态字节带压缩方式标识的打包 / 拆包。</summary>
    public static class RelayCompressors
    {
        /// <summary>RFC 1951 原始 deflate（.NET DeflateStream），与 Go SDK 的 relay.Deflate（compress/flate）互通。</summary>
        public static readonly IRelayCompressor Deflate = new DeflateCompressor();
        /// <summary>原样发出、原样交出。</summary>
        public static readonly IRelayCompressor None = new NoCompressor();
        /// <summary>YouAreHost 里状态字节解压后的上限（与 Go SDK 的 relay.MaxStateBytes 相同）。</summary>
        public const int MaxStateBytes = 16 << 20;
        /// <summary>内置压缩方式的标识。</summary>
        public const byte NoneId = 0, DeflateId = 1;

        /// <summary>把状态打成线上的字节：[压缩方式标识 1B][压缩后的字节]；空状态就是空的（不带标识）。</summary>
        public static byte[] Pack(IRelayCompressor compressor, ReadOnlySpan<byte> state)
        {
            if (state.IsEmpty) return Array.Empty<byte>();
            var packed = compressor.Compress(state);
            var output = new byte[1 + packed.Length];
            output[0] = compressor.Id;
            packed.CopyTo(output, 1);
            return output;
        }

        /// <summary>
        /// Pack 的反面：按标识选解压方式（自己的 compressor 优先，再是内置的两种），解出的字节不超过 limit。标识不认识抛
        /// <see cref="RelayUnknownCompressionException"/>；标识认识、内容解不开抛解压错误（InvalidDataException）。空的就是空状态。
        /// </summary>
        public static byte[] Unpack(IRelayCompressor compressor, ReadOnlySpan<byte> data, int limit)
        {
            if (data.IsEmpty) return Array.Empty<byte>();
            byte id = data[0];
            IRelayCompressor method;
            if (id == compressor.Id) method = compressor;
            else if (id == DeflateId) method = Deflate;
            else if (id == NoneId) method = None;
            else throw new RelayUnknownCompressionException(id, compressor.Id);
            return method.Decompress(data.Slice(1), limit);
        }

        private sealed class DeflateCompressor : IRelayCompressor
        {
            public byte Id => DeflateId;
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
            public byte Id => NoneId;
            public byte[] Compress(ReadOnlySpan<byte> data) => data.ToArray();

            public byte[] Decompress(ReadOnlySpan<byte> data, int limit)
            {
                if (data.Length > limit) throw new InvalidDataException($"Relay state exceeds {limit} bytes.");
                return data.ToArray();
            }
        }
    }
}
