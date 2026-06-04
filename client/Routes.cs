namespace SgateClient;

public static class Routes
{
    public static class Client
    {
        public const string Handshake = "handshake";
        public const string Login = "login";
        public const string Ping = "ping";
        public const string Pong = "pong";
        public const string Test = "test";
        public const string TestResult = "testResult";
        public const string Version = "version";
        public const string GetConnections = "getConnections";
        public const string Broadcast = "broadcast";
        public const string Health = "health";
        public const string APIDocs = "api-docs";
        public const string Error = "error";
        public const string Echo = "echo";
        public const string Message = "message";
        public const string Kick = "kick";
        public const string Timeout = "timeout";
        public const string HandshakeResponse = "handshake_response";
        public const string QueueTest = "queueTest";
        public const string AddWhitelist = "addWhitelist";
        public const string RemoveWhitelist = "removeWhitelist";
        public const string GetWhitelist = "getWhitelist";
        public const string AddBlacklist = "addBlacklist";
        public const string RemoveBlacklist = "removeBlacklist";
        public const string GetBlacklist = "getBlacklist";
    }

    public static class Server
    {
        public const string Kick = "server.kick";
        public const string JoinGroup = "server.join_group";
        public const string LeaveGroup = "server.leave_group";
        public const string JoinGroupByUser = "server.join_group_by_user";
        public const string LeaveGroupByUser = "server.leave_group_by_user";
        public const string CreateGroup = "server.create_group";
        public const string DeleteGroup = "server.delete_group";
        public const string SendToGroup = "server.send_to_group";
        public const string GetGroupInfo = "server.get_group_info";
        public const string PlayerOnline = "server.playerOnline";
        public const string PlayerOffline = "server.playerOffline";
        public const string PlayerMoved = "server.playerMoved";
        public const string Chat = "server.chat";
        public const string Push = "server.push";
        public const string Announcement = "server.announcement";
        public const string Announce = "server.announce";
        public const string RoomPlayerJoined = "server.room.playerJoined";
        public const string RoomPlayerLeft = "server.room.playerLeft";
        public const string TeamMemberJoined = "server.team.memberJoined";
        public const string TeamMemberLeft = "server.team.memberLeft";
        public const string DamageNotify = "server.damageNotify";
        public const string AttackBroadcast = "server.attackBroadcast";
    }

    public static class Player
    {
        public const string Login = "player.login";
        public const string Heartbeat = "player.heartbeat";
        public const string Move = "player.move";
        public const string Chat = "player.chat";
        public const string Attack = "player.attack";
        public const string UseItem = "player.useItem";
        public const string QueryStatus = "player.queryStatus";
        public const string QueryOnline = "player.queryOnline";
    }

    public static class Room
    {
        public const string Join = "room.join";
        public const string Leave = "room.leave";
        public const string Info = "room.info";
    }

    public static class Team
    {
        public const string Create = "team.create";
        public const string Join = "team.join";
        public const string Leave = "team.leave";
        public const string Info = "team.info";
    }
}
