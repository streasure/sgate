using System.Collections.Concurrent;
using System.Net.Sockets;
using System.Security.Cryptography;
using System.Text;
using Google.Protobuf;
using SgateClient.Protobuf;

namespace SgateClient;

public class SgateClientOptions
{
    public string Host { get; set; } = "127.0.0.1";
    public int Port { get; set; } = 48080;
    public string ServerId { get; set; } = "S1";
    public string UserId { get; set; } = "";
    public string Token { get; set; } = "";
    public string ProtocolVersion { get; set; } = "2.0.0";
    public string ClientType { get; set; } = "desktop";
    public string ClientVersion { get; set; } = "1.0.0";
    public string DeviceId { get; set; } = "";
    public TimeSpan ConnectTimeout { get; set; } = TimeSpan.FromSeconds(10);
    public TimeSpan ReadTimeout { get; set; } = TimeSpan.FromSeconds(30);
}

public class SgateConnection : IDisposable
{
    private readonly SgateClientOptions _options;
    private TcpClient? _tcpClient;
    private NetworkStream? _stream;
    private CancellationTokenSource? _cts;
    private Task? _receiveTask;
    private readonly ConcurrentDictionary<string, Func<Message, Task>> _routeHandlers = new();
    private readonly ConcurrentDictionary<string, TaskCompletionSource<Message>> _pendingRequests = new();
    private int _sequence;
    private bool _disposed;

    public string ConnectionId { get; private set; } = "";
    public string UserUuid { get; private set; } = "";
    public string NegotiatedVersion { get; private set; } = "";
    public bool IsConnected => _tcpClient?.Connected == true;

    public event Func<Message, Task>? OnMessage;
    public event Func<ErrorResponse, Task>? OnError;
    public event Func<Task>? OnDisconnected;

    public SgateConnection(SgateClientOptions? options = null)
    {
        _options = options ?? new SgateClientOptions();
    }

    public async Task ConnectAsync(CancellationToken cancellationToken = default)
    {
        _tcpClient = new TcpClient();
        await _tcpClient.ConnectAsync(_options.Host, _options.Port, cancellationToken);
        _stream = _tcpClient.GetStream();
        _stream.ReadTimeout = (int)_options.ReadTimeout.TotalMilliseconds;
        _cts = new CancellationTokenSource();

        _receiveTask = ReceiveLoopAsync(_cts.Token);
    }

    public async Task HandshakeAsync(CancellationToken cancellationToken = default)
    {
        var handshake = new Handshake
        {
            ProtocolVersion = _options.ProtocolVersion,
            ClientType = _options.ClientType,
            ClientVersion = _options.ClientVersion,
            DeviceId = _options.DeviceId,
            Timestamp = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds(),
        };
        handshake.SupportedVersions.Add(_options.ProtocolVersion);

        var handshakeBytes = handshake.ToByteArray();
        var handshakeBase64 = Convert.ToBase64String(handshakeBytes);

        var msg = new Message
        {
            Route = Routes.Client.Handshake,
            ProtocolVersion = _options.ProtocolVersion,
            Timestamp = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds(),
        };
        msg.Payload["handshake_data"] = handshakeBase64;
        msg.Payload["version"] = _options.ProtocolVersion;
        msg.Payload["timestamp"] = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds().ToString();
        msg.Payload["serverId"] = _options.ServerId;

        var response = await SendAndWaitAsync(msg, Routes.Client.HandshakeResponse, cancellationToken);

        if (response.Payload.TryGetValue("negotiated_version", out var version))
            NegotiatedVersion = version;
    }

    public async Task LoginAsync(CancellationToken cancellationToken = default)
    {
        var msg = new Message
        {
            Route = Routes.Client.Login,
            Timestamp = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds(),
        };
        msg.Payload["userId"] = _options.UserId;
        msg.Payload["token"] = _options.Token;
        msg.Payload["serverId"] = _options.ServerId;

        var response = await SendAndWaitAsync(msg, cancellationToken: cancellationToken);

        if (response.Payload.TryGetValue("code", out var code) && code != "200")
        {
            var errMsg = response.Payload.TryGetValue("message", out var m) ? m : "Login failed";
            throw new Exception($"Login failed: {errMsg} (code={code})");
        }

        ConnectionId = response.ConnectionId;
        UserUuid = response.UserUuid;
    }

    public async Task ConnectAndLoginAsync(CancellationToken cancellationToken = default)
    {
        await ConnectAsync(cancellationToken);
        await HandshakeAsync(cancellationToken);
        await LoginAsync(cancellationToken);
    }

    private static readonly Dictionary<string, string> ResponseRouteMap = new()
    {
        [Routes.Client.Ping] = Routes.Client.Pong,
    };

    public Task<Message> SendAsync(string route, Dictionary<string, string>? payload = null, CancellationToken cancellationToken = default)
    {
        var msg = new Message
        {
            Route = route,
            Timestamp = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds(),
            Sequence = Interlocked.Increment(ref _sequence),
        };
        if (payload != null)
        {
            foreach (var kv in payload)
                msg.Payload[kv.Key] = kv.Value;
        }
        var expectedRoute = ResponseRouteMap.GetValueOrDefault(route, route);
        return SendAndWaitAsync(msg, expectedRoute: expectedRoute, cancellationToken: cancellationToken);
    }

    public async Task SendNoWaitAsync(string route, Dictionary<string, string>? payload = null, CancellationToken cancellationToken = default)
    {
        var msg = new Message
        {
            Route = route,
            Timestamp = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds(),
            Sequence = Interlocked.Increment(ref _sequence),
        };
        if (payload != null)
        {
            foreach (var kv in payload)
                msg.Payload[kv.Key] = kv.Value;
        }
        await WriteMessageAsync(msg, cancellationToken);
    }

    public void On(string route, Func<Message, Task> handler)
    {
        _routeHandlers[route] = handler;
    }

    public void On(string route, Action<Message> handler)
    {
        _routeHandlers[route] = msg => { handler(msg); return Task.CompletedTask; };
    }

    private async Task<Message> SendAndWaitAsync(Message msg, string? expectedRoute = null, CancellationToken cancellationToken = default)
    {
        var seq = msg.Sequence;
        var tcs = new TaskCompletionSource<Message>();
        var key = seq > 0 ? $"{msg.Route}:{seq}" : msg.Route;

        if (expectedRoute != null)
            key = expectedRoute;

        _pendingRequests[key] = tcs;

        try
        {
            await WriteMessageAsync(msg, cancellationToken);

            using var cts = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
            cts.CancelAfter(_options.ReadTimeout);
            try
            {
                return await tcs.Task.WaitAsync(cts.Token);
            }
            catch (OperationCanceledException) when (!cancellationToken.IsCancellationRequested)
            {
                throw new TimeoutException($"Timeout waiting for response to route '{msg.Route}'");
            }
        }
        finally
        {
            _pendingRequests.TryRemove(key, out _);
        }
    }

    private async Task WriteMessageAsync(Message msg, CancellationToken cancellationToken = default)
    {
        if (_stream == null) throw new InvalidOperationException("Not connected");
        PrepareMessage(msg);
        var frame = FrameCodec.EncodeMessage(msg);
        await _stream.WriteAsync(frame, cancellationToken);
        await _stream.FlushAsync(cancellationToken);
    }

    private static void PrepareMessage(Message msg)
    {
        msg.Timestamp = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds();
        if (string.IsNullOrEmpty(msg.ProtocolVersion))
            msg.ProtocolVersion = "2.0.0";
        msg.Checksum = GenerateMessageChecksum(msg);
    }

    private static string GenerateMessageChecksum(Message msg)
    {
        var sb = new StringBuilder();
        sb.Append(msg.ConnectionId);
        sb.Append('|');
        sb.Append(msg.UserUuid);
        sb.Append('|');
        sb.Append(msg.Route);
        sb.Append('|');

        var keys = msg.Payload.Keys.OrderBy(k => k).ToList();
        foreach (var k in keys)
        {
            sb.Append(k);
            sb.Append('=');
            sb.Append(msg.Payload[k]);
            sb.Append('|');
        }

        sb.Append(msg.Timestamp);
        sb.Append('|');
        sb.Append(msg.Sequence);
        sb.Append('|');
        sb.Append(msg.ProtocolVersion);

        var raw = sb.ToString();
        var hash = MD5.HashData(Encoding.UTF8.GetBytes(raw));
        return Convert.ToHexString(hash).ToLowerInvariant();
    }

    private async Task ReceiveLoopAsync(CancellationToken cancellationToken)
    {
        try
        {
            while (!cancellationToken.IsCancellationRequested && _stream != null)
            {
                var payload = await FrameCodec.ReadFrameAsync(_stream, cancellationToken);
                var (message, isError) = FrameCodec.DecodePayload(payload);

                if (isError && message is ErrorResponse errResp)
                {
                    if (OnError != null)
                        await OnError(errResp);
                    continue;
                }

                if (message is Message msg)
                {
                    await DispatchMessageAsync(msg);
                }
            }
        }
        catch (EndOfStreamException)
        {
        }
        catch (OperationCanceledException)
        {
        }
        catch (Exception ex)
        {
            Console.WriteLine($"[SgateConnection] Receive error: {ex.Message}");
        }
        finally
        {
            if (OnDisconnected != null)
                await OnDisconnected();
        }
    }

    private async Task DispatchMessageAsync(Message msg)
    {
        if (OnMessage != null)
            await OnMessage(msg);

        if (_pendingRequests.TryRemove(msg.Route, out var tcs))
        {
            tcs.TrySetResult(msg);
            return;
        }

        var seqKey = $"{msg.Route}:{msg.Sequence}";
        if (_pendingRequests.TryRemove(seqKey, out var tcs2))
        {
            tcs2.TrySetResult(msg);
            return;
        }

        if (_routeHandlers.TryGetValue(msg.Route, out var handler))
        {
            try
            {
                await handler(msg);
            }
            catch (Exception ex)
            {
                Console.WriteLine($"[SgateConnection] Handler error for route '{msg.Route}': {ex.Message}");
            }
        }
    }

    public void Dispose()
    {
        if (_disposed) return;
        _disposed = true;

        _cts?.Cancel();
        _stream?.Close();
        _tcpClient?.Close();
        _cts?.Dispose();
        _stream?.Dispose();
        _tcpClient?.Dispose();

        foreach (var tcs in _pendingRequests.Values)
            tcs.TrySetCanceled();

        _pendingRequests.Clear();
    }
}
