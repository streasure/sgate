using System.Buffers.Binary;
using System.IO;
using Google.Protobuf;
using SgateClient.Protobuf;

namespace SgateClient;

public static class FrameCodec
{
    public static readonly int HeaderSize = 4;

    public static byte[] EncodeMessage(Message msg)
    {
        var payload = msg.ToByteArray();
        var frame = new byte[HeaderSize + payload.Length];
        BinaryPrimitives.WriteUInt32BigEndian(frame, (uint)payload.Length);
        Buffer.BlockCopy(payload, 0, frame, HeaderSize, payload.Length);
        return frame;
    }

    public static byte[] EncodeMessage(ErrorResponse msg)
    {
        var payload = msg.ToByteArray();
        var frame = new byte[HeaderSize + payload.Length];
        BinaryPrimitives.WriteUInt32BigEndian(frame, (uint)payload.Length);
        Buffer.BlockCopy(payload, 0, frame, HeaderSize, payload.Length);
        return frame;
    }

    public static async Task<byte[]> ReadFrameAsync(Stream stream, CancellationToken cancellationToken = default)
    {
        var header = new byte[HeaderSize];
        await ReadExactAsync(stream, header, 0, HeaderSize, cancellationToken);

        var payloadSize = (int)BinaryPrimitives.ReadUInt32BigEndian(header);
        if (payloadSize <= 0 || payloadSize > 16 * 1024 * 1024)
            throw new InvalidDataException($"Invalid payload size: {payloadSize}");

        var payload = new byte[payloadSize];
        await ReadExactAsync(stream, payload, 0, payloadSize, cancellationToken);
        return payload;
    }

    public static (object message, bool isError) DecodePayload(byte[] payload)
    {
        try
        {
            var msg = Message.Parser.ParseFrom(payload);
            if (msg.Route == Routes.Client.Error || string.IsNullOrEmpty(msg.Route))
            {
                try
                {
                    var errResp = ErrorResponse.Parser.ParseFrom(payload);
                    return (errResp, true);
                }
                catch
                {
                    return (msg, false);
                }
            }
            return (msg, false);
        }
        catch
        {
            try
            {
                var errResp = ErrorResponse.Parser.ParseFrom(payload);
                return (errResp, true);
            }
            catch
            {
                throw new InvalidDataException("Failed to decode payload as Message or ErrorResponse");
            }
        }
    }

    private static async Task ReadExactAsync(Stream stream, byte[] buffer, int offset, int count, CancellationToken cancellationToken)
    {
        int totalRead = 0;
        while (totalRead < count)
        {
            var bytesRead = await stream.ReadAsync(buffer.AsMemory(offset + totalRead, count - totalRead), cancellationToken);
            if (bytesRead == 0)
                throw new EndOfStreamException("Connection closed by remote");
            totalRead += bytesRead;
        }
    }
}
