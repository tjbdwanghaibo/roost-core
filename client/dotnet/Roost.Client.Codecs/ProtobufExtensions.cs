using System;
using System.Threading;
using Google.Protobuf;

namespace Roost.Client.Codecs
{
    public static class ProtobufExtensions
    {
        // 默认 DAO PB 的 patch 应用器由生成器生成；Full 时 previous 为 null。
        public static void RegisterDao<T>(this EntityViews views, string ns, MessageParser<T> parser,
            Func<T, T?, T> apply, string profile = "self", uint schema = 0) where T : class, IMessage<T>
        {
            if (parser == null || apply == null) throw new ArgumentNullException(nameof(parser));
            views.Register<T>(ns, profile, 2, schema, (update, previous) => apply(parser.ParseFrom(update.Payload), previous));
        }

        public static void Request<T>(this GameClient client, uint requestId, uint responseId, IMessage request,
            MessageParser<T> parser, Action<T> received, Action<Exception> failed, CancellationToken cancellation = default)
            where T : IMessage<T>
        {
            if (request == null || parser == null) throw new ArgumentNullException(nameof(request));
            client.Request(requestId, responseId, request.ToByteArray(), parser.ParseFrom, received, failed, cancellation);
        }
        public static IDisposable Subscribe<T>(this GameClient client, uint messageId, MessageParser<T> parser, Action<T> received)
            where T : IMessage<T>
        {
            if (parser == null) throw new ArgumentNullException(nameof(parser));
            return client.Subscribe(messageId, parser.ParseFrom, received);
        }
    }
}
