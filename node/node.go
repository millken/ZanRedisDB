package node

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/absolute8511/ZanRedisDB/common"
	"github.com/absolute8511/ZanRedisDB/rockredis"
	"github.com/absolute8511/ZanRedisDB/store"
	"github.com/coreos/etcd/pkg/wait"
	"github.com/coreos/etcd/raft"
	"github.com/coreos/etcd/raft/raftpb"
	"github.com/tidwall/redcon"
)

var (
	errInvalidResponse  = errors.New("Invalid response type")
	errSyntaxError      = errors.New("syntax error")
	errUnknownData      = errors.New("unknown request data type")
	errTooMuchBatchSize = errors.New("the batch size exceed the limit")
)

const (
	RedisReq int8 = 0
	HTTPReq  int8 = 1
)

type nodeProgress struct {
	confState raftpb.ConfState
	snapi     uint64
	appliedi  uint64
}

type internalReq struct {
	reqData InternalRaftRequest
	done    chan struct{}
}

// a key-value node backed by raft
type KVNode struct {
	reqProposeC       chan *internalReq
	proposeC          chan<- []byte // channel for proposing updates
	raftNode          *raftNode
	store             *store.KVStore
	stopping          int32
	stopChan          chan struct{}
	w                 wait.Wait
	router            *common.CmdRouter
	deleteCb          func()
	dbWriteStats      common.WriteStats
	clusterWriteStats common.WriteStats
	ns                string
	nodeConfig        *NodeConfig
}

type KVSnapInfo struct {
	*rockredis.BackupInfo
	BackupMeta []byte        `json:"backup_meta"`
	LeaderInfo *MemberInfo   `json:"leader_info"`
	Members    []*MemberInfo `json:"members"`
}

func (self *KVSnapInfo) GetData() ([]byte, error) {
	meta, err := self.BackupInfo.GetResult()
	if err != nil {
		return nil, err
	}
	self.BackupMeta = meta
	d, _ := json.Marshal(self)
	return d, nil
}

func NewKVNode(kvopts *store.KVOptions, nodeConfig *NodeConfig,
	ns string, clusterID uint64, id int, localRaftAddr string,
	peers map[int]string, join bool, deleteCb func()) (*KVNode, chan raftpb.ConfChange) {
	proposeC := make(chan []byte)
	confChangeC := make(chan raftpb.ConfChange)

	config := &RaftConfig{
		ClusterID:   clusterID,
		Namespace:   ns,
		ID:          id,
		RaftAddr:    localRaftAddr,
		DataDir:     kvopts.DataDir,
		RaftPeers:   peers,
		SnapCount:   kvopts.SnapCount,
		SnapCatchup: kvopts.SnapCatchup,
		nodeConfig:  nodeConfig,
	}
	config.WALDir = path.Join(config.DataDir, fmt.Sprintf("wal-%d", id))
	config.SnapDir = path.Join(config.DataDir, fmt.Sprintf("snap-%d", id))

	s := &KVNode{
		reqProposeC: make(chan *internalReq, 200),
		proposeC:    proposeC,
		store:       store.NewKVStore(kvopts),
		stopChan:    make(chan struct{}),
		w:           wait.New(),
		router:      common.NewCmdRouter(),
		deleteCb:    deleteCb,
		ns:          ns,
		nodeConfig:  nodeConfig,
	}
	s.registerHandler()
	commitC, errorC, raftNode := newRaftNode(config,
		join, s, proposeC, confChangeC)
	s.raftNode = raftNode

	raftNode.startRaft(s)
	// read commits from raft into KVStore map until error
	go s.applyCommits(commitC, errorC)
	go s.handleProposeReq()
	return s, confChangeC
}

func (self *KVNode) Stop() {
	if !atomic.CompareAndSwapInt32(&self.stopping, 0, 1) {
		return
	}
	self.raftNode.StopNode()
	self.store.Close()
	close(self.stopChan)
	go self.deleteCb()
}

func (self *KVNode) OptimizeDB() {
	self.store.CompactRange()
}

func (self *KVNode) GetLeadMember() *MemberInfo {
	return self.raftNode.GetLeadMember()
}
func (self *KVNode) GetMembers() []*MemberInfo {
	return self.raftNode.GetMembers()
}

func (self *KVNode) GetStats() common.NamespaceStats {
	tbs := self.store.GetTables()
	var ns common.NamespaceStats
	ns.DBWriteStats = self.dbWriteStats.Copy()
	ns.ClusterWriteStats = self.clusterWriteStats.Copy()
	ns.InternalStats = self.store.GetInternalStatus()

	for t := range tbs {
		cnt, err := self.store.GetTableKeyCount(t)
		if err != nil {
			continue
		}
		var ts common.TableStats
		ts.Name = string(t)
		ts.KeyNum = cnt
		ns.TStats = append(ns.TStats, ts)
	}
	nodeLog.Info(self.store.GetStatistics())
	return ns
}

func (self *KVNode) Clear() error {
	return self.store.Clear()
}

func (self *KVNode) GetHandler(cmd string) (common.CommandFunc, bool) {
	return self.router.GetCmdHandler(cmd)
}

func (self *KVNode) registerHandler() {
	// for kv
	self.router.Register("get", wrapReadCommandK(self.getCommand))
	self.router.Register("mget", wrapReadCommandKK(self.mgetCommand))
	self.router.Register("exists", wrapReadCommandK(self.existsCommand))
	self.router.Register("set", wrapWriteCommandKV(self, self.setCommand))
	self.router.Register("setnx", wrapWriteCommandKV(self, self.setnxCommand))
	self.router.Register("mset", wrapWriteCommandKVKV(self, self.msetCommand))
	self.router.Register("incr", wrapWriteCommandK(self, self.incrCommand))
	self.router.Register("del", wrapWriteCommandKK(self, self.delCommand))
	self.router.Register("plget", self.plgetCommand)
	self.router.Register("plset", self.plsetCommand)
	// for hash
	self.router.Register("hget", wrapReadCommandKSubkey(self.hgetCommand))
	self.router.Register("hgetall", wrapReadCommandK(self.hgetallCommand))
	self.router.Register("hkeys", wrapReadCommandK(self.hkeysCommand))
	self.router.Register("hexists", wrapReadCommandKSubkey(self.hexistsCommand))
	self.router.Register("hmget", wrapReadCommandKSubkeySubkey(self.hmgetCommand))
	self.router.Register("hlen", wrapReadCommandK(self.hlenCommand))
	self.router.Register("hset", wrapWriteCommandKSubkeyV(self, self.hsetCommand))
	self.router.Register("hmset", wrapWriteCommandKSubkeyVSubkeyV(self, self.hmsetCommand))
	self.router.Register("hdel", wrapWriteCommandKSubkeySubkey(self, self.hdelCommand))
	self.router.Register("hincrby", wrapWriteCommandKSubkeyV(self, self.hincrbyCommand))
	self.router.Register("hclear", wrapWriteCommandK(self, self.hclearCommand))
	// for list
	self.router.Register("lindex", wrapReadCommandKSubkey(self.lindexCommand))
	self.router.Register("llen", wrapReadCommandK(self.llenCommand))
	self.router.Register("lrange", wrapReadCommandKAnySubkey(self.lrangeCommand))
	self.router.Register("lpop", wrapWriteCommandK(self, self.lpopCommand))
	self.router.Register("lpush", wrapWriteCommandKVV(self, self.lpushCommand))
	self.router.Register("lset", self.lsetCommand)
	self.router.Register("ltrim", self.ltrimCommand)
	self.router.Register("rpop", wrapWriteCommandK(self, self.rpopCommand))
	self.router.Register("rpush", wrapWriteCommandKVV(self, self.rpushCommand))
	self.router.Register("lclear", wrapWriteCommandK(self, self.lclearCommand))
	// for zset
	self.router.Register("zscore", wrapReadCommandKSubkey(self.zscoreCommand))
	self.router.Register("zcount", wrapReadCommandKAnySubkey(self.zcountCommand))
	self.router.Register("zcard", wrapReadCommandK(self.zcardCommand))
	self.router.Register("zlexcount", wrapReadCommandKAnySubkey(self.zlexcountCommand))
	self.router.Register("zrange", wrapReadCommandKAnySubkey(self.zrangeCommand))
	self.router.Register("zrevrange", wrapReadCommandKAnySubkey(self.zrevrangeCommand))
	self.router.Register("zrangebylex", wrapReadCommandKAnySubkey(self.zrangebylexCommand))
	self.router.Register("zrangebyscore", wrapReadCommandKAnySubkey(self.zrangebyscoreCommand))
	self.router.Register("zrevrangebyscore", wrapReadCommandKAnySubkey(self.zrevrangebyscoreCommand))
	self.router.Register("zrank", wrapReadCommandKSubkey(self.zrankCommand))
	self.router.Register("zrevrank", wrapReadCommandKSubkey(self.zrevrankCommand))
	self.router.Register("zadd", self.zaddCommand)
	self.router.Register("zincrby", self.zincrbyCommand)
	self.router.Register("zrem", wrapWriteCommandKSubkeySubkey(self, self.zremCommand))
	self.router.Register("zremrangebyrank", self.zremrangebyrankCommand)
	self.router.Register("zremrangebyscore", self.zremrangebyscoreCommand)
	self.router.Register("zremrangebylex", self.zremrangebylexCommand)
	self.router.Register("zclear", wrapWriteCommandK(self, self.zclearCommand))
	// for set
	self.router.Register("scard", wrapReadCommandK(self.scardCommand))
	self.router.Register("sismember", wrapReadCommandKSubkey(self.sismemberCommand))
	self.router.Register("smembers", wrapReadCommandK(self.smembersCommand))
	self.router.Register("sadd", wrapWriteCommandKSubkeySubkey(self, self.saddCommand))
	self.router.Register("srem", wrapWriteCommandKSubkeySubkey(self, self.sremCommand))
	self.router.Register("sclear", wrapWriteCommandK(self, self.sclearCommand))
	self.router.Register("smclear", wrapWriteCommandKK(self, self.smclearCommand))

	// for scan
	self.router.Register("scan", wrapReadCommandKAnySubkey(self.scanCommand))
	self.router.Register("hscan", wrapReadCommandKAnySubkey(self.hscanCommand))
	self.router.Register("sscan", wrapReadCommandKAnySubkey(self.sscanCommand))
	self.router.Register("zscan", wrapReadCommandKAnySubkey(self.zscanCommand))
	self.router.Register("advscan", self.advanceScanCommand)

	// only write command need to be registered as internal
	// kv
	self.router.RegisterInternal("del", self.localDelCommand)
	self.router.RegisterInternal("set", self.localSetCommand)
	self.router.RegisterInternal("setnx", self.localSetnxCommand)
	self.router.RegisterInternal("mset", self.localMSetCommand)
	self.router.RegisterInternal("incr", self.localIncrCommand)
	self.router.RegisterInternal("plset", self.localPlsetCommand)
	// hash
	self.router.RegisterInternal("hset", self.localHSetCommand)
	self.router.RegisterInternal("hmset", self.localHMsetCommand)
	self.router.RegisterInternal("hdel", self.localHDelCommand)
	self.router.RegisterInternal("hincrby", self.localHIncrbyCommand)
	self.router.RegisterInternal("hclear", self.localHclearCommand)
	// list
	self.router.RegisterInternal("lpop", self.localLpopCommand)
	self.router.RegisterInternal("lpush", self.localLpushCommand)
	self.router.RegisterInternal("lset", self.localLsetCommand)
	self.router.RegisterInternal("ltrim", self.localLtrimCommand)
	self.router.RegisterInternal("rpop", self.localRpopCommand)
	self.router.RegisterInternal("rpush", self.localRpushCommand)
	self.router.RegisterInternal("lclear", self.localLclearCommand)
	// zset
	self.router.RegisterInternal("zadd", self.localZaddCommand)
	self.router.RegisterInternal("zincrby", self.localZincrbyCommand)
	self.router.RegisterInternal("zrem", self.localZremCommand)
	self.router.RegisterInternal("zremrangebyrank", self.localZremrangebyrankCommand)
	self.router.RegisterInternal("zremrangebyscore", self.localZremrangebyscoreCommand)
	self.router.RegisterInternal("zremrangebylex", self.localZremrangebylexCommand)
	self.router.RegisterInternal("zclear", self.localZclearCommand)
	// set
	self.router.RegisterInternal("sadd", self.localSadd)
	self.router.RegisterInternal("srem", self.localSrem)
	self.router.RegisterInternal("sclear", self.localSclear)
	self.router.RegisterInternal("smclear", self.localSmclear)
}

func (self *KVNode) handleProposeReq() {
	var reqList BatchInternalRaftRequest
	reqList.Reqs = make([]*InternalRaftRequest, 0, 100)
	var lastReq *internalReq
	defer func() {
		if e := recover(); e != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			buf = buf[0:n]
			nodeLog.Infof("handle propose loop panic: %s:%v", buf, e)
		}
		nodeLog.Infof("handle propose loop exit")
		for _, r := range reqList.Reqs {
			self.w.Trigger(r.Header.ID, common.ErrStopped)
		}
		for {
			select {
			case r := <-self.reqProposeC:
				self.w.Trigger(r.reqData.Header.ID, common.ErrStopped)
			default:
				break
			}
		}
	}()
	for {
		select {
		case r := <-self.reqProposeC:
			reqList.Reqs = append(reqList.Reqs, &r.reqData)
			lastReq = r
		default:
			if len(reqList.Reqs) == 0 {
				select {
				case r := <-self.reqProposeC:
					reqList.Reqs = append(reqList.Reqs, &r.reqData)
					lastReq = r
				case <-self.stopChan:
					return
				}
			}
			reqList.ReqNum = int32(len(reqList.Reqs))
			buffer, err := reqList.Marshal()
			if err != nil {
				nodeLog.Infof("failed to marshal request: %v", err)
				for _, r := range reqList.Reqs {
					self.w.Trigger(r.Header.ID, err)
				}
				reqList.Reqs = reqList.Reqs[:0]
				continue
			}
			lastReq.done = make(chan struct{})
			//nodeLog.Infof("handle req %v, marshal buffer: %v, raw: %v, %v", len(reqList.Reqs),
			//	realN, buffer, reqList.Reqs)
			start := time.Now()
			self.proposeC <- buffer
			select {
			case <-lastReq.done:
			case <-self.stopChan:
				return
			}
			cost := time.Since(start)
			if len(reqList.Reqs) >= 100 && cost >= time.Second || (cost >= time.Second*2) {
				nodeLog.Infof("slow for batch: %v, %v", len(reqList.Reqs), cost)
			}
			reqList.Reqs = reqList.Reqs[:0]
			lastReq = nil
		}
	}
}

func (self *KVNode) queueRequest(req *internalReq) (interface{}, error) {
	start := time.Now()
	ch := self.w.Register(req.reqData.Header.ID)
	select {
	case self.reqProposeC <- req:
	default:
		select {
		case self.reqProposeC <- req:
		case <-self.stopChan:
			self.w.Trigger(req.reqData.Header.ID, common.ErrStopped)
		case <-time.After(time.Second * 3):
			self.w.Trigger(req.reqData.Header.ID, common.ErrTimeout)
		}
	}
	//nodeLog.Infof("queue request: %v", req.reqData.String())
	var err error
	var rsp interface{}
	var ok bool
	select {
	case rsp = <-ch:
		if req.done != nil {
			close(req.done)
		}
		if err, ok = rsp.(error); ok {
			rsp = nil
		} else {
			err = nil
		}
	case <-self.stopChan:
		rsp = nil
		err = common.ErrStopped
	}
	self.clusterWriteStats.UpdateWriteStats(int64(len(req.reqData.Data)), time.Since(start).Nanoseconds()/1000)
	return rsp, err
}

func (self *KVNode) Propose(buf []byte) (interface{}, error) {
	h := &RequestHeader{
		ID:       self.raftNode.reqIDGen.Next(),
		DataType: 0,
	}
	raftReq := InternalRaftRequest{
		Header: h,
		Data:   buf,
	}
	req := &internalReq{
		reqData: raftReq,
	}
	return self.queueRequest(req)
}

func (self *KVNode) HTTPPropose(buf []byte) (interface{}, error) {
	h := &RequestHeader{
		ID:       self.raftNode.reqIDGen.Next(),
		DataType: int32(HTTPReq),
	}
	raftReq := InternalRaftRequest{
		Header: h,
		Data:   buf,
	}
	req := &internalReq{
		reqData: raftReq,
	}
	return self.queueRequest(req)
}

func (self *KVNode) applySnapshot(np *nodeProgress, applyEvent *applyInfo) {
	if raft.IsEmptySnap(applyEvent.snapshot) {
		return
	}
	// signaled to load snapshot
	nodeLog.Infof("applying snapshot at index %d, snapshot: %v\n", np.snapi, applyEvent.snapshot.String())
	defer nodeLog.Infof("finished applying snapshot at index %d\n", np)

	if applyEvent.snapshot.Metadata.Index <= np.appliedi {
		nodeLog.Fatalf("snapshot index [%d] should > progress.appliedIndex [%d] + 1",
			applyEvent.snapshot.Metadata.Index, np.appliedi)
	}

	if err := self.RestoreFromSnapshot(false, applyEvent.snapshot); err != nil {
		nodeLog.Panic(err)
	}

	np.confState = applyEvent.snapshot.Metadata.ConfState
	np.snapi = applyEvent.snapshot.Metadata.Index
	np.appliedi = applyEvent.snapshot.Metadata.Index
}

func (self *KVNode) applyAll(np *nodeProgress, applyEvent *applyInfo) bool {
	self.applySnapshot(np, applyEvent)
	if len(applyEvent.ents) == 0 {
		return false
	}
	firsti := applyEvent.ents[0].Index
	if firsti > np.appliedi+1 {
		nodeLog.Panicf("first index of committed entry[%d] should <= appliedi[%d] + 1", firsti, np.appliedi)
	}
	var ents []raftpb.Entry
	if np.appliedi+1-firsti < uint64(len(applyEvent.ents)) {
		ents = applyEvent.ents[np.appliedi+1-firsti:]
	}
	if len(ents) == 0 {
		return false
	}
	var shouldStop bool
	var confChanged bool
	for i := range ents {
		evnt := ents[i]
		switch evnt.Type {
		case raftpb.EntryNormal:
			if evnt.Data != nil {
				start := time.Now()
				// try redis command
				var reqList BatchInternalRaftRequest
				parseErr := reqList.Unmarshal(evnt.Data)
				if parseErr != nil {
					nodeLog.Infof("parse request failed: %v, data len %v, entry: %v, raw:%v",
						parseErr, len(evnt.Data), evnt,
						string(evnt.Data))
				}
				if len(reqList.Reqs) != int(reqList.ReqNum) {
					nodeLog.Infof("request check failed %v, real len:%v",
						reqList, len(reqList.Reqs))
				}
				for _, req := range reqList.Reqs {
					reqID := req.Header.ID
					if req.Header.DataType == 0 {
						cmd, err := redcon.Parse(req.Data)
						if err != nil {
							self.w.Trigger(reqID, err)
						} else {
							cmdName := strings.ToLower(string(cmd.Args[0]))
							h, ok := self.router.GetInternalCmdHandler(cmdName)
							if !ok {
								nodeLog.Infof("unsupported redis command: %v", cmd)
								self.w.Trigger(reqID, common.ErrInvalidCommand)
							} else {
								cmdStart := time.Now()
								v, err := h(cmd)
								cmdCost := time.Since(cmdStart)
								if cmdCost >= time.Millisecond*500 {
									nodeLog.Infof("slow write command: %v, cost: %v", string(cmd.Raw), cmdCost)
								}
								self.dbWriteStats.UpdateWriteStats(int64(len(cmd.Raw)), cmdCost.Nanoseconds()/1000)
								// write the future response or error
								if err != nil {
									self.w.Trigger(reqID, err)
								} else {
									self.w.Trigger(reqID, v)
								}
							}
						}
					} else if req.Header.DataType == int32(HTTPReq) {
						//TODO: try other protocol command
						self.w.Trigger(reqID, errUnknownData)
					} else {
						self.w.Trigger(reqID, errUnknownData)
					}
				}
				cost := time.Since(start)
				if len(reqList.Reqs) >= 100 && cost > time.Second || (cost > time.Second*2) {
					nodeLog.Infof("slow for batch write db: %v, %v", len(reqList.Reqs), cost)
				}

			}
		case raftpb.EntryConfChange:
			var cc raftpb.ConfChange
			cc.Unmarshal(evnt.Data)
			removeSelf, _ := self.raftNode.applyConfChange(cc, &np.confState)
			shouldStop = shouldStop || removeSelf
			confChanged = true
		}
		np.appliedi = evnt.Index
		if evnt.Index == self.raftNode.lastIndex {
			nodeLog.Infof("replay finished at index: %v\n", evnt.Index)
		}
	}
	if shouldStop {
		go func() {
			time.Sleep(time.Second)
			select {
			case self.raftNode.errorC <- errors.New("my node removed"):
			default:
			}
		}()
	}
	return confChanged
}

func (self *KVNode) applyCommits(commitC <-chan applyInfo, errorC <-chan error) {
	defer func() {
		self.Stop()
	}()
	snap, err := self.raftNode.raftStorage.Snapshot()
	if err != nil {
		panic(err)
	}
	np := nodeProgress{
		confState: snap.Metadata.ConfState,
		snapi:     snap.Metadata.Index,
		appliedi:  snap.Metadata.Index,
	}
	nodeLog.Infof("starting state: %v\n", np)
	for {
		select {
		case ent := <-commitC:
			confChanged := self.applyAll(&np, &ent)
			<-ent.raftDone
			self.maybeTriggerSnapshot(&np, confChanged)
			self.raftNode.handleSendSnapshot(&np)
		case err, ok := <-errorC:
			if !ok {
				return
			}
			nodeLog.Infof("error: %v", err)
			return
		case <-self.stopChan:
			return
		}
	}
}

func (self *KVNode) maybeTriggerSnapshot(np *nodeProgress, confChanged bool) {
	if np.appliedi-np.snapi <= 0 {
		return
	}
	if !confChanged && np.appliedi-np.snapi <= uint64(self.raftNode.config.SnapCount) {
		return
	}
	if np.appliedi <= self.raftNode.lastIndex {
		// replaying local log
		return
	}

	nodeLog.Infof("start snapshot [applied index: %d | last snapshot index: %d]", np.appliedi, np.snapi)
	err := self.raftNode.beginSnapshot(np.appliedi, np.confState)
	if err != nil {
		nodeLog.Infof("begin snapshot failed: %v", err)
		return
	}

	np.snapi = np.appliedi
}

func (self *KVNode) GetSnapshot(term uint64, index uint64) (Snapshot, error) {
	// use the rocksdb backup/checkpoint interface to backup data
	var si KVSnapInfo
	si.BackupInfo = self.store.Backup(term, index)
	if si.BackupInfo == nil {
		return nil, errors.New("failed to begin backup: maybe too much backup running")
	}
	si.WaitReady()
	si.LeaderInfo = self.raftNode.GetLeadMember()
	si.Members = self.raftNode.GetMembers()
	return &si, nil
}

func (self *KVNode) RestoreFromSnapshot(startup bool, raftSnapshot raftpb.Snapshot) error {
	snapshot := raftSnapshot.Data
	var si KVSnapInfo
	err := json.Unmarshal(snapshot, &si)
	if err != nil {
		return err
	}
	self.raftNode.RestoreMembers(si.Members)
	nodeLog.Infof("should recovery from snapshot here: %v", raftSnapshot.String())
	// while startup we can use the local snapshot to restart,
	// but while running, we should install the leader's snapshot,
	// so we need remove local and sync from leader
	hasBackup, _ := self.checkLocalBackup(raftSnapshot)
	if !startup {
		// TODO: currently, we use the backup id as meta, this can be
		// the same even the snap applied index is different, so we can not
		// tell if the local backup id is exactly the desired snap.
		// we need clear and copy from remote.
		// In order to avoid this, we need write some meta to backup,
		// such as write snap term and index to backup, or name the backup id
		// using the snap term+index
		// hasBackup = false
	}
	if !hasBackup {
		nodeLog.Infof("local no backup for snapshot, copy from remote\n")
		syncAddr, syncDir := self.GetValidBackupInfo(raftSnapshot)
		if syncAddr == "" && syncDir == "" {
			panic("no backup can be found from others")
		}
		// copy backup data from the remote leader node, and recovery backup from it
		// if local has some old backup data, we should use rsync to sync the data file
		// use the rocksdb backup/checkpoint interface to backup data
		common.RunFileSync(syncAddr,
			path.Join(rockredis.GetBackupDir(syncDir),
				rockredis.GetCheckpointDir(raftSnapshot.Metadata.Term, raftSnapshot.Metadata.Index)),
			self.store.GetBackupDir())
	}
	return self.store.Restore(raftSnapshot.Metadata.Term, raftSnapshot.Metadata.Index)
}

func (self *KVNode) CheckLocalBackup(snapData []byte) (bool, error) {
	var rs raftpb.Snapshot
	err := rs.Unmarshal(snapData)
	if err != nil {
		return false, err
	}
	return self.checkLocalBackup(rs)
}

func (self *KVNode) checkLocalBackup(rs raftpb.Snapshot) (bool, error) {
	var si KVSnapInfo
	err := json.Unmarshal(rs.Data, &si)
	if err != nil {
		nodeLog.Infof("unmarshal snap meta failed: %v", string(rs.Data))
		return false, err
	}
	return self.store.IsLocalBackupOK(rs.Metadata.Term, rs.Metadata.Index)
}

type deadlinedConn struct {
	Timeout time.Duration
	net.Conn
}

func (c *deadlinedConn) Read(b []byte) (n int, err error) {
	c.Conn.SetReadDeadline(time.Now().Add(c.Timeout))
	return c.Conn.Read(b)
}

func (c *deadlinedConn) Write(b []byte) (n int, err error) {
	c.Conn.SetWriteDeadline(time.Now().Add(c.Timeout))
	return c.Conn.Write(b)
}

func newDeadlineTransport(timeout time.Duration) *http.Transport {
	transport := &http.Transport{
		Dial: func(netw, addr string) (net.Conn, error) {
			c, err := net.DialTimeout(netw, addr, timeout)
			if err != nil {
				return nil, err
			}
			return &deadlinedConn{timeout, c}, nil
		},
	}
	return transport
}

func (self *KVNode) GetValidBackupInfo(raftSnapshot raftpb.Snapshot) (string, string) {
	// we need find the right backup data match with the raftsnapshot
	// for each cluster member, it need check the term+index and the backup meta to
	// make sure the data is valid
	snapshot := raftSnapshot.Data
	var si KVSnapInfo
	err := json.Unmarshal(snapshot, &si)
	if err != nil {
		return "", ""
	}
	remoteLeader := si.LeaderInfo
	members := make([]*MemberInfo, 0)
	members = append(members, remoteLeader)
	members = append(members, si.Members...)
	curMembers := self.raftNode.GetMembers()
	members = append(members, curMembers...)
	syncAddr := ""
	syncDir := ""
	h := self.nodeConfig.BroadcastAddr
	for _, m := range members {
		if m == nil {
			continue
		}
		if m.ID == uint64(self.raftNode.config.ID) {
			continue
		}
		c := http.Client{Transport: newDeadlineTransport(time.Second)}
		body, _ := raftSnapshot.Marshal()
		req, _ := http.NewRequest("GET", "http://"+m.Broadcast+":"+
			strconv.Itoa(m.HttpAPIPort)+"/cluster/checkbackup/"+self.ns, bytes.NewBuffer(body))
		rsp, err := c.Do(req)
		if err != nil {
			nodeLog.Infof("request error: %v", err)
			continue
		}
		rsp.Body.Close()
		if m.Broadcast == h {
			if m.DataDir == self.store.GetBackupBase() {
				// the leader is old mine, try find another leader
				nodeLog.Infof("data dir can not be same if on local: %v, %v", m, self.store.GetBackupBase())
				continue
			}
			// local node with different directory
			syncAddr = ""
		} else {
			syncAddr = m.Broadcast
		}
		syncDir = m.DataDir
		break
	}
	nodeLog.Infof("should recovery from : %v, %v", syncAddr, syncDir)
	return syncAddr, syncDir
}
