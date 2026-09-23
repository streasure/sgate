package internal

import (
	"context"
	"hash/fnv"
	"runtime"
	"sync"
	"sync/atomic"

	protoGw "github.com/streasure/protocol/gateway"
	"github.com/streasure/sgate/internal/config"
	"github.com/streasure/util/tlog"

	"github.com/panjf2000/gnet/v2"
	"google.golang.org/protobuf/proto"
)

// pipelineTask 是提交给 worker pool 的异步任务（TCP 或 WS）。
type pipelineTask struct {
	task   *pipelineTaskData
	wsTask *wsPipelineTaskData
}

// pipelineTaskData TCP pipeline 任务数据。
type pipelineTaskData struct {
	conn         gnet.Conn
	data         []byte
	message      *protoGw.StreamData
	connectionID string
	remoteIP     string
}

// wsPipelineTaskData WebSocket pipeline 任务数据。
type wsPipelineTaskData struct {
	wsConn       *WebSocketConnection
	payload      []byte
	message      *protoGw.StreamData
	connectionID string
}

// pipelineWorker 是单个 worker goroutine，处理一个分片内的所有连接。
type pipelineWorker struct {
	taskCh chan pipelineTask
}

// PipelineWorkerPool 是分片的异步 pipeline 工作池。
// 按 connectionID 哈希分片，保证同一连接的消息严格有序。
type PipelineWorkerPool struct {
	workers  []pipelineWorker
	shards   int
	gw       *Gateway
	submitted atomic.Int64
	dropped   atomic.Int64
	wg        sync.WaitGroup
}

// NewPipelineWorkerPool 创建并启动异步 pipeline 工作池。
// shardCount 应为 2 的幂以优化哈希取模。
func NewPipelineWorkerPool(gw *Gateway, cfg config.PipelineConfig) *PipelineWorkerPool {
	shards := cfg.WorkerShards
	if shards <= 0 {
		shards = runtime.GOMAXPROCS(0) * 4
	}
	queueSize := cfg.WorkerQueueSize
	if queueSize <= 0 {
		queueSize = 65536
	}

	p := &PipelineWorkerPool{
		workers: make([]pipelineWorker, shards),
		shards:  shards,
		gw:      gw,
	}

	for i := 0; i < shards; i++ {
		p.workers[i].taskCh = make(chan pipelineTask, queueSize/shards)
		p.wg.Add(1)
		go p.runWorker(i)
	}

	tlog.Info(context.Background(), "pipeline worker pool started shards=%d queueSize=%d", shards, queueSize)
	return p
}

// runWorker 是单个 worker goroutine 的主循环。
func (p *PipelineWorkerPool) runWorker(shard int) {
	defer p.wg.Done()
	for t := range p.workers[shard].taskCh {
		if t.wsTask != nil {
			p.processWSTask(t.wsTask)
		} else {
			p.processTask(t.task)
		}
	}
}

// processTask 在 worker goroutine 中执行 TCP pipeline 处理。
func (p *PipelineWorkerPool) processTask(task *pipelineTaskData) {
	result := p.gw.pipeline.Process(task.conn, task.data, task.message, task.connectionID)
	if result.Error != nil {
		errorResp := newErrorResponse("error", result.Error.Error(), "", "")
		respData, _ := proto.Marshal(errorResp)
		writeFrame(task.conn, respData)
	}
}

// processWSTask 在 worker goroutine 中执行 WebSocket pipeline 处理。
func (p *PipelineWorkerPool) processWSTask(task *wsPipelineTaskData) {
	result := p.gw.pipeline.ProcessForWS(task.wsConn.Conn, task.payload, task.message, task.connectionID)
	if result.Error != nil {
		errorResp := newErrorResponse("error", result.Error.Error(), "", "")
		responseData := marshalClientError(errorResp)
		p.gw.sendWebSocketMessage(task.wsConn, WSOpBinary, responseData)
	}
}

// Submit 将 TCP pipeline 任务提交到对应分片的 worker。
func (p *PipelineWorkerPool) Submit(task pipelineTaskData) bool {
	p.submitted.Add(1)
	shard := p.getShard(task.connectionID)
	select {
	case p.workers[shard].taskCh <- pipelineTask{task: &task}:
		return true
	default:
		p.dropped.Add(1)
		tlog.Warn(context.Background(), "pipeline worker queue full, task dropped shard=%d connectionID=%s", shard, task.connectionID)
		return false
	}
}

// SubmitWS 将 WebSocket pipeline 任务提交到对应分片的 worker。
func (p *PipelineWorkerPool) SubmitWS(task wsPipelineTaskData) bool {
	p.submitted.Add(1)
	shard := p.getShard(task.connectionID)
	select {
	case p.workers[shard].taskCh <- pipelineTask{wsTask: &task}:
		return true
	default:
		p.dropped.Add(1)
		tlog.Warn(context.Background(), "pipeline worker queue full, WS task dropped shard=%d connectionID=%s", shard, task.connectionID)
		return false
	}
}

// getShard 根据 connectionID 计算分片索引。
func (p *PipelineWorkerPool) getShard(connectionID string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(connectionID))
	return h.Sum32() % uint32(p.shards)
}

// Stats 返回工作池统计信息。
func (p *PipelineWorkerPool) Stats() (submitted, dropped int64, queueDepth int64) {
	submitted = p.submitted.Load()
	dropped = p.dropped.Load()
	var depth int64
	for i := range p.workers {
		depth += int64(len(p.workers[i].taskCh))
	}
	return submitted, dropped, depth
}

// Stop 停止工作池，等待所有 worker 退出。
func (p *PipelineWorkerPool) Stop() {
	for i := range p.workers {
		close(p.workers[i].taskCh)
	}
	p.wg.Wait()
}
