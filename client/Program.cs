using SgateClient;
using SgateClient.Protobuf;

if (args.Length < 1)
{
    Console.WriteLine("Usage: SgateClient <host:port> [serverId] [userId]");
    Console.WriteLine("Example: SgateClient 127.0.0.1:48080 S1 user1");
    return;
}

var parts = args[0].Split(':');
var host = parts[0];
var port = parts.Length > 1 ? int.Parse(parts[1]) : 48080;
var serverId = args.Length > 1 ? args[1] : "S1";
var userId = args.Length > 2 ? args[2] : "csharp_user";

var options = new SgateClientOptions
{
    Host = host,
    Port = port,
    ServerId = serverId,
    UserId = userId,
    Token = "test-token",
    DeviceId = Guid.NewGuid().ToString("N")[..16],
};

using var client = new SgateConnection(options);

client.OnError += err =>
{
    var code = err.Error?.Code ?? "unknown";
    var message = err.Error?.Message ?? "unknown error";
    Console.WriteLine($"[ERROR] code={code} message={message}");
    return Task.CompletedTask;
};

client.OnDisconnected += () =>
{
    Console.WriteLine("[DISCONNECTED] Connection lost");
    return Task.CompletedTask;
};

client.On(Routes.Server.Kick, msg =>
{
    Console.WriteLine($"[KICKED] {string.Join(", ", msg.Payload.Select(p => $"{p.Key}={p.Value}"))}");
    return Task.CompletedTask;
});

client.On(Routes.Server.Announce, msg =>
{
    Console.WriteLine($"[ANNOUNCEMENT] {string.Join(", ", msg.Payload.Select(p => $"{p.Key}={p.Value}"))}");
    return Task.CompletedTask;
});

client.On(Routes.Server.Chat, msg =>
{
    Console.WriteLine($"[CHAT] {string.Join(", ", msg.Payload.Select(p => $"{p.Key}={p.Value}"))}");
    return Task.CompletedTask;
});

try
{
    Console.WriteLine($"Connecting to {host}:{port} ...");
    await client.ConnectAsync();
    Console.WriteLine("Connected!");

    Console.WriteLine("Performing handshake...");
    await client.HandshakeAsync();
    Console.WriteLine($"Handshake OK, negotiated version: {client.NegotiatedVersion}");

    Console.WriteLine("Logging in...");
    try
    {
        await client.LoginAsync();
        Console.WriteLine($"Login OK, connectionId: {client.ConnectionId}");
    }
    catch (Exception ex)
    {
        Console.WriteLine($"Login skipped: {ex.Message}");
    }

    while (true)
    {
        Console.WriteLine();
        Console.WriteLine("=== Sgate Client Menu ===");
        Console.WriteLine("1. Ping");
        Console.WriteLine("2. Echo");
        Console.WriteLine("3. Test");
        Console.WriteLine("4. Get Connections");
        Console.WriteLine("5. Player Move");
        Console.WriteLine("6. Player Chat");
        Console.WriteLine("7. Room Join");
        Console.WriteLine("8. Room Leave");
        Console.WriteLine("9. Team Create");
        Console.WriteLine("0. Exit");
        Console.Write("Select: ");

        var input = Console.ReadLine()?.Trim();
        if (input == "0" || input == "") break;

        try
        {
            var sw = System.Diagnostics.Stopwatch.StartNew();

            switch (input)
            {
                case "1":
                    {
                        var resp = await client.SendAsync(Routes.Client.Ping);
                        sw.Stop();
                        Console.WriteLine($"[PONG] {FormatPayload(resp)} latency={sw.ElapsedMilliseconds}ms");
                    }
                    break;

                case "2":
                    {
                        Console.Write("Message: ");
                        var echoMsg = Console.ReadLine() ?? "hello from C#";
                        var resp = await client.SendAsync(Routes.Client.Echo, new Dictionary<string, string>
                        {
                            ["message"] = echoMsg,
                        });
                        sw.Stop();
                        Console.WriteLine($"[ECHO] {FormatPayload(resp)} latency={sw.ElapsedMilliseconds}ms");
                    }
                    break;

                case "3":
                    {
                        Console.Write("Data: ");
                        var testData = Console.ReadLine() ?? "test from C#";
                        var resp = await client.SendAsync(Routes.Client.Test, new Dictionary<string, string>
                        {
                            ["data"] = testData,
                        });
                        sw.Stop();
                        Console.WriteLine($"[TEST] {FormatPayload(resp)} latency={sw.ElapsedMilliseconds}ms");
                    }
                    break;

                case "4":
                    {
                        var resp = await client.SendAsync(Routes.Client.GetConnections);
                        sw.Stop();
                        Console.WriteLine($"[CONNECTIONS] {FormatPayload(resp)} latency={sw.ElapsedMilliseconds}ms");
                    }
                    break;

                case "5":
                    {
                        var rand = new Random();
                        var posX = rand.NextDouble() * 1000;
                        var posY = rand.NextDouble() * 1000;
                        var resp = await client.SendAsync(Routes.Player.Move, new Dictionary<string, string>
                        {
                            ["posX"] = posX.ToString("F1"),
                            ["posY"] = posY.ToString("F1"),
                        });
                        sw.Stop();
                        Console.WriteLine($"[MOVE] {FormatPayload(resp)} latency={sw.ElapsedMilliseconds}ms");
                    }
                    break;

                case "6":
                    {
                        Console.Write("Chat message: ");
                        var chatMsg = Console.ReadLine() ?? "hello from C# client";
                        var resp = await client.SendAsync(Routes.Player.Chat, new Dictionary<string, string>
                        {
                            ["message"] = chatMsg,
                        });
                        sw.Stop();
                        Console.WriteLine($"[CHAT] {FormatPayload(resp)} latency={sw.ElapsedMilliseconds}ms");
                    }
                    break;

                case "7":
                    {
                        Console.Write("Room ID: ");
                        var roomId = Console.ReadLine() ?? "room1";
                        var resp = await client.SendAsync(Routes.Room.Join, new Dictionary<string, string>
                        {
                            ["roomID"] = roomId,
                        });
                        sw.Stop();
                        Console.WriteLine($"[ROOM JOIN] {FormatPayload(resp)} latency={sw.ElapsedMilliseconds}ms");
                    }
                    break;

                case "8":
                    {
                        Console.Write("Room ID: ");
                        var roomId = Console.ReadLine() ?? "room1";
                        var resp = await client.SendAsync(Routes.Room.Leave, new Dictionary<string, string>
                        {
                            ["roomID"] = roomId,
                        });
                        sw.Stop();
                        Console.WriteLine($"[ROOM LEAVE] {FormatPayload(resp)} latency={sw.ElapsedMilliseconds}ms");
                    }
                    break;

                case "9":
                    {
                        Console.Write("Team name: ");
                        var teamName = Console.ReadLine() ?? "team1";
                        var resp = await client.SendAsync(Routes.Team.Create, new Dictionary<string, string>
                        {
                            ["teamName"] = teamName,
                        });
                        sw.Stop();
                        Console.WriteLine($"[TEAM CREATE] {FormatPayload(resp)} latency={sw.ElapsedMilliseconds}ms");
                    }
                    break;

                default:
                    Console.WriteLine("Unknown option");
                    break;
            }
        }
        catch (TimeoutException ex)
        {
            Console.WriteLine($"[TIMEOUT] {ex.Message}");
        }
        catch (Exception ex)
        {
            Console.WriteLine($"[ERROR] {ex.Message}");
        }
    }
}
catch (Exception ex)
{
    Console.WriteLine($"Fatal error: {ex.Message}");
}

static string FormatPayload(Message msg)
{
    var parts = msg.Payload.Select(p => $"{p.Key}={p.Value}");
    return $"route={msg.Route} [{string.Join(", ", parts)}]";
}
