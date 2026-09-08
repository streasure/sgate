############本文件禁止修改########################
############本文件禁止修改########################
############本文件禁止修改########################
#通信流程细节
1.客户端先于sgate建立tcp或者websocket链接
2.客户端向sgate发送logingatereq，认证完成，session设置为可用状态，并将userUuid指向特定的serverId(session什么时候创建参考，和useruuid绑定直接参考D:\server\tech-center\gateserver中的实现)
3.可以进行客户端->sagte->logic的双向通信。
其中客户端和sgate的收发消息体都为messageframe
而sgate和logic之间的交互消息全是streamdata


#节点注册信息
sgate和logic在启动之后就向etcd注册自己节点信息用来给其他服务做服务发现和管理。
etcd上会带节点的serverType，serverid，serverzone，ip，port等信息。
这些信息除了ip动态取服务器本地其他在各自的配置文件中都会有。

#配置相关
配置文件中有所需支持插件(prometheus，grafana，etcd，redis，mysql等被用到的)的配置
serverType，serverId，serverZone
对外长连接端口
对外grpc端口
pprof端口

#功能支持
tcp，websocket长连接通信
sgate内部管理用户组，理论上也是可以用来作为聊天组推送的。
sgate提供给logic端的组加入，组退出，组退出。全服推送也是一种组，只是玩家在sgate登录校验通过后就直接默认加入serverId所在的组。
玩家离线无论是走的主动的logoutreq还是断线，都会自动退出他所加入的所有组。这个过程无需logic触发，直接在sgate内部处理完所有流程。走异步，不卡整个组管理的流程。


#其他细节：
当前实现的logic不需要解析sgate收到的消息，只需要在收到之后原样返还给sgate即可，不过需要支持哪个玩家来的发给哪个玩家。
logicserver内部只需要实现简单的定时的组推送，单玩家推送和主动地组加入和退出操作。验证logicserver在推送向的性能。推送的消息就默认心跳即可。
logic向sgate的推送没有session这个概念。只走userUuid和groupId。
组推送的话理论来说只需要logic向sgate调用一个通用的grpc接口，随后在sgate层做数据的分发处理


