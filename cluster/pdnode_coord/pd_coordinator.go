package pdnode_coord

import (
	"errors"
	. "github.com/absolute8511/ZanRedisDB/cluster"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrAlreadyExist    = errors.New("already exist")
	ErrNotLeader       = errors.New("Not leader")
	ErrClusterUnstable = errors.New("the cluster is unstable")

	ErrLeaderNodeLost            = NewCoordErr("leader node is lost", CoordTmpErr)
	ErrNodeNotFound              = NewCoordErr("node not found", CoordCommonErr)
	ErrNodeUnavailable           = NewCoordErr("No node is available for namespace", CoordTmpErr)
	ErrNamespaceRaftEnough       = NewCoordErr("the namespace isr and catchup nodes are enough", CoordTmpErr)
	ErrClusterNodeRemoving       = NewCoordErr("the node is mark as removed", CoordTmpErr)
	ErrNamespaceNodeConflict     = NewCoordErr("the namespace node info is conflicted", CoordClusterErr)
	ErrRegisterServiceUnstable   = NewCoordErr("the register service is unstable", CoordTmpErr)
	ErrNamespaceReplicaNotEnough = NewCoordErr("the replica nodes is not enough", CoordTmpErr)
)

const (
	waitMigrateInterval          = time.Minute * 3
	waitEmergencyMigrateInterval = time.Second * 30
)

type PDCoordinator struct {
	register               PDRegister
	balanceWaiting         int32
	clusterKey             string
	myNode                 NodeInfo
	leaderNode             NodeInfo
	nodesMutex             sync.RWMutex
	dataNodes              map[string]NodeInfo
	removingNodes          map[string]string
	nodesEpoch             int64
	checkNamespaceFailChan chan NamespaceNameInfo
	stopChan               chan struct{}
	wg                     sync.WaitGroup
	monitorChan            chan struct{}
	isClusterUnstable      int32
	isUpgrading            int32
	dpm                    *DataPlacement
	doChecking             int32
}

func NewPDCoordinator(cluster string, n *NodeInfo, opts *Options) *PDCoordinator {
	coord := &PDCoordinator{
		clusterKey:             cluster,
		myNode:                 *n,
		register:               nil,
		dataNodes:              make(map[string]NodeInfo),
		removingNodes:          make(map[string]string),
		checkNamespaceFailChan: make(chan NamespaceNameInfo, 3),
		stopChan:               make(chan struct{}),
		monitorChan:            make(chan struct{}),
	}
	coord.dpm = NewDataPlacement(coord)
	if opts != nil {
		coord.dpm.SetBalanceInterval(opts.BalanceStart, opts.BalanceEnd)
	}
	return coord
}

func (self *PDCoordinator) SetRegister(r PDRegister) {
	self.register = r
}

func (self *PDCoordinator) Start() error {
	if self.register != nil {
		self.register.InitClusterID(self.clusterKey)
		err := self.register.Register(&self.myNode)
		if err != nil {
			CoordLog().Warningf("failed to register pd coordinator: %v", err)
			return err
		}
	}
	self.wg.Add(1)
	go self.handleLeadership()
	return nil
}

func (self *PDCoordinator) Stop() {
	close(self.stopChan)
	if self.register != nil {
		self.register.Unregister(&self.myNode)
		self.register.Stop()
	}
	self.wg.Wait()
	CoordLog().Infof("coordinator stopped.")
}

func (self *PDCoordinator) handleLeadership() {
	defer self.wg.Done()
	leaderChan := make(chan *NodeInfo)
	if self.register != nil {
		go self.register.AcquireAndWatchLeader(leaderChan, self.stopChan)
	}
	defer func() {
		if e := recover(); e != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			buf = buf[0:n]
			CoordLog().Errorf("panic %s:%v", buf, e)
		}

		CoordLog().Warningf("leadership watch exit.")
		if self.monitorChan != nil {
			close(self.monitorChan)
			self.monitorChan = nil
		}
	}()
	for {
		select {
		case l, ok := <-leaderChan:
			if !ok {
				CoordLog().Warningf("leader chan closed.")
				return
			}
			if l == nil {
				CoordLog().Warningf("leader is lost.")
				continue
			}
			if l.GetID() != self.leaderNode.GetID() ||
				l.Epoch != self.leaderNode.Epoch {
				CoordLog().Infof("leader changed from %v to %v", self.leaderNode, *l)
				self.leaderNode = *l
				if self.leaderNode.GetID() != self.myNode.GetID() {
					// remove watchers.
					if self.monitorChan != nil {
						close(self.monitorChan)
					}
					self.monitorChan = make(chan struct{})
				}
				self.notifyLeaderChanged(self.monitorChan)
			}
			if self.leaderNode.GetID() == "" {
				CoordLog().Warningf("leader is missing.")
			}
		}
	}
}

func (self *PDCoordinator) notifyLeaderChanged(monitorChan chan struct{}) {
	if self.leaderNode.GetID() != self.myNode.GetID() {
		CoordLog().Infof("I am slave (%v). Leader is: %v", self.myNode, self.leaderNode)
		self.nodesMutex.Lock()
		self.removingNodes = make(map[string]string)
		self.nodesMutex.Unlock()
		return
	}
	CoordLog().Infof("I am master now.")
	// reload namespace information
	if self.register != nil {
		newNamespaces, _, err := self.register.GetAllNamespaces()
		if err != nil {
			CoordLog().Errorf("load namespace info failed: %v", err)
		} else {
			CoordLog().Infof("namespace loaded : %v", len(newNamespaces))
		}
	}

	self.wg.Add(1)
	go func() {
		defer self.wg.Done()
		self.handleDataNodes(monitorChan)
	}()
	self.wg.Add(1)
	go func() {
		defer self.wg.Done()
		self.checkNamespaces(monitorChan)
	}()
	self.wg.Add(1)
	go func() {
		defer self.wg.Done()
		self.dpm.DoBalance(monitorChan)
	}()
	self.wg.Add(1)
	go func() {
		defer self.wg.Done()
		self.handleRemovingNodes(monitorChan)
	}()
}

func (self *PDCoordinator) getCurrentNodes(tags map[string]bool) map[string]NodeInfo {
	self.nodesMutex.RLock()
	currentNodes := self.dataNodes
	if len(self.removingNodes) > 0 || len(tags) > 0 {
		currentNodes = make(map[string]NodeInfo)
		for nid, n := range self.dataNodes {
			if _, ok := self.removingNodes[nid]; ok {
				continue
			}
			filtered := false
			for tag, _ := range tags {
				if _, ok := n.Tags[tag]; !ok {
					filtered = true
					break
				}
			}
			if filtered {
				continue
			}
			currentNodes[nid] = n
		}
	}
	self.nodesMutex.RUnlock()
	return currentNodes
}

func (self *PDCoordinator) getCurrentNodesWithRemoving() (map[string]NodeInfo, int64) {
	self.nodesMutex.RLock()
	currentNodes := self.dataNodes
	currentNodesEpoch := atomic.LoadInt64(&self.nodesEpoch)
	self.nodesMutex.RUnlock()
	return currentNodes, currentNodesEpoch
}

func (self *PDCoordinator) getCurrentNodesWithEpoch(tags map[string]bool) (map[string]NodeInfo, int64) {
	self.nodesMutex.RLock()
	currentNodes := self.dataNodes
	if len(self.removingNodes) > 0 || len(tags) > 0 {
		currentNodes = make(map[string]NodeInfo)
		for nid, n := range self.dataNodes {
			if _, ok := self.removingNodes[nid]; ok {
				continue
			}
			filtered := false
			for tag, _ := range tags {
				if _, ok := n.Tags[tag]; !ok {
					filtered = true
					break
				}
			}
			if filtered {
				continue
			}

			currentNodes[nid] = n
		}
	}
	currentNodesEpoch := atomic.LoadInt64(&self.nodesEpoch)
	self.nodesMutex.RUnlock()
	return currentNodes, currentNodesEpoch
}

func (self *PDCoordinator) handleDataNodes(monitorChan chan struct{}) {
	nodesChan := make(chan []NodeInfo)
	if self.register != nil {
		go self.register.WatchDataNodes(nodesChan, monitorChan)
	}
	CoordLog().Debugf("start watch the nodes.")
	defer func() {
		CoordLog().Infof("stop watch the nodes.")
	}()
	for {
		select {
		case nodes, ok := <-nodesChan:
			if !ok {
				return
			}
			// check if any node changed.
			CoordLog().Debugf("Current data nodes: %v", len(nodes))
			oldNodes := self.dataNodes
			newNodes := make(map[string]NodeInfo)
			for _, v := range nodes {
				//CoordLog().Infof("node %v : %v", v.GetID(), v)
				newNodes[v.GetID()] = v
			}
			self.nodesMutex.Lock()
			self.dataNodes = newNodes
			check := false
			for oldID, oldNode := range oldNodes {
				if _, ok := newNodes[oldID]; !ok {
					CoordLog().Warningf("node failed: %v, %v", oldID, oldNode)
					// if node is missing we need check election immediately.
					check = true
				}
			}
			// failed need be protected by lock so we can avoid contention.
			if check {
				atomic.AddInt64(&self.nodesEpoch, 1)
			}
			self.nodesMutex.Unlock()

			if self.register == nil {
				continue
			}
			for newID, newNode := range newNodes {
				if _, ok := oldNodes[newID]; !ok {
					CoordLog().Infof("new node joined: %v, %v", newID, newNode)
					check = true
				}
			}
			if check {
				atomic.AddInt64(&self.nodesEpoch, 1)
				atomic.StoreInt32(&self.isClusterUnstable, 1)
				self.triggerCheckNamespaces("", 0, time.Millisecond*10)
			}
		}
	}
}

func (self *PDCoordinator) handleRemovingNodes(monitorChan chan struct{}) {
	CoordLog().Debugf("start handle the removing nodes.")
	defer func() {
		CoordLog().Infof("stop handle the removing nodes.")
	}()
	ticker := time.NewTicker(time.Second * 30)
	defer ticker.Stop()
	for {
		select {
		case <-monitorChan:
			return
		case <-ticker.C:
			//
			anyStateChanged := false
			self.nodesMutex.RLock()
			removingNodes := make(map[string]string)
			for nid, removeState := range self.removingNodes {
				removingNodes[nid] = removeState
			}
			self.nodesMutex.RUnlock()
			// remove state: marked -> pending -> data_transfered -> done
			if len(removingNodes) == 0 {
				continue
			}
			currentNodes := self.getCurrentNodes(nil)
			nodeNameList := make([]string, 0, len(currentNodes))
			for _, s := range currentNodes {
				nodeNameList = append(nodeNameList, s.ID)
			}

			allNamespaces, _, err := self.register.GetAllNamespaces()
			if err != nil {
				continue
			}
			for nid, _ := range removingNodes {
				anyPending := false
				CoordLog().Infof("handle removing node %v ", nid)
				// only check the namespace with one replica left
				// because the doCheckNamespaces will check the others
				// we add a new replica for the removing node
				for _, namespacePartList := range allNamespaces {
					for _, namespaceInfo := range namespacePartList {
						if FindSlice(namespaceInfo.RaftNodes, nid) == -1 {
							continue
						}
						if len(namespaceInfo.RaftNodes) <= namespaceInfo.Replica {
							anyPending = true
							// find new catchup and wait isr ready
							removingNodes[nid] = "pending"
							err := self.dpm.addNodeToNamespaceAndWaitReady(monitorChan, &namespaceInfo,
								nodeNameList)
							if err != nil {
								CoordLog().Infof("namespace %v data on node %v transfered failed, waiting next time", namespaceInfo.GetDesp(), nid)
								continue
							}
							CoordLog().Infof("namespace %v data on node %v transfered success", namespaceInfo.GetDesp(), nid)
							anyStateChanged = true
						}
						self.removeNamespaceFromNode(&namespaceInfo, nid)
					}
					if !anyPending {
						anyStateChanged = true
						CoordLog().Infof("node %v data has been transfered, it can be removed from cluster: state: %v", nid, removingNodes[nid])
						if removingNodes[nid] != "data_transfered" && removingNodes[nid] != "done" {
							removingNodes[nid] = "data_transfered"
						} else {
							if removingNodes[nid] == "data_transfered" {
								removingNodes[nid] = "done"
							} else if removingNodes[nid] == "done" {
								self.nodesMutex.Lock()
								_, ok := self.dataNodes[nid]
								if !ok {
									delete(removingNodes, nid)
									CoordLog().Infof("the node %v is removed finally since not alive in cluster", nid)
								}
								self.nodesMutex.Unlock()
							}
						}
					}
				}
			}

			if anyStateChanged {
				self.nodesMutex.Lock()
				self.removingNodes = removingNodes
				self.nodesMutex.Unlock()
			}
		}
	}
}

func (self *PDCoordinator) triggerCheckNamespaces(namespace string, part int, delay time.Duration) {
	time.Sleep(delay)

	select {
	case self.checkNamespaceFailChan <- NamespaceNameInfo{namespace, part}:
	case <-self.stopChan:
		return
	case <-time.After(time.Second):
		return
	}
}

// check if partition is enough,
// check if replication is enough
// check any unexpected state.
func (self *PDCoordinator) checkNamespaces(monitorChan chan struct{}) {
	ticker := time.NewTicker(time.Second * 30)
	waitingMigrateNamespace := make(map[string]map[int]time.Time)
	defer func() {
		ticker.Stop()
		CoordLog().Infof("check namespaces quit.")
	}()

	for {
		select {
		case <-monitorChan:
			return
		case <-ticker.C:
			if self.register == nil {
				continue
			}
			self.doCheckNamespaces(monitorChan, nil, waitingMigrateNamespace, true)
		case failedInfo := <-self.checkNamespaceFailChan:
			if self.register == nil {
				continue
			}
			self.doCheckNamespaces(monitorChan, &failedInfo, waitingMigrateNamespace, failedInfo.NamespaceName == "")
		}
	}
}

func (self *PDCoordinator) doCheckNamespaces(monitorChan chan struct{}, failedInfo *NamespaceNameInfo,
	waitingMigrateNamespace map[string]map[int]time.Time, fullCheck bool) {

	if !atomic.CompareAndSwapInt32(&self.doChecking, 0, 1) {
		return
	}
	time.Sleep(time.Millisecond * 10)
	defer atomic.StoreInt32(&self.doChecking, 0)

	namespaces := []PartitionMetaInfo{}
	if failedInfo == nil || failedInfo.NamespaceName == "" || failedInfo.NamespacePartition < 0 {
		allNamespaces, _, commonErr := self.register.GetAllNamespaces()
		if commonErr != nil {
			CoordLog().Infof("scan namespaces failed. %v", commonErr)
			return
		}
		for _, parts := range allNamespaces {
			for _, p := range parts {
				namespaces = append(namespaces, p)
			}
		}
		CoordLog().Debugf("scan found namespaces: %v", namespaces)
	} else {
		var err error
		CoordLog().Infof("check single namespace : %v ", failedInfo)
		var t *PartitionMetaInfo
		t, err = self.register.GetNamespacePartInfo(failedInfo.NamespaceName, failedInfo.NamespacePartition)
		if err != nil {
			CoordLog().Infof("get namespace info failed: %v, %v", failedInfo, err)
			return
		}
		namespaces = append(namespaces, *t)
	}

	// TODO: check partition number for namespace, maybe failed to create
	// some partition when creating namespace.

	currentNodes, currentNodesEpoch := self.getCurrentNodesWithRemoving()
	CoordLog().Infof("do check namespaces (%v), current nodes: %v, ...", len(namespaces), len(currentNodes))
	checkOK := true
	for _, nsInfo := range namespaces {
		if currentNodesEpoch != atomic.LoadInt64(&self.nodesEpoch) {
			CoordLog().Infof("nodes changed while checking namespaces: %v, %v", currentNodesEpoch, atomic.LoadInt64(&self.nodesEpoch))
			return
		}
		select {
		case <-monitorChan:
			// exiting
			return
		default:
		}

		needMigrate := false
		if len(nsInfo.RaftNodes) < nsInfo.Replica {
			CoordLog().Infof("replicas is not enough for namespace %v, isr is :%v", nsInfo.GetDesp(), nsInfo.RaftNodes)
			needMigrate = true
			checkOK = false
		}

		aliveCount := 0
		for _, replica := range nsInfo.RaftNodes {
			if _, ok := currentNodes[replica]; !ok {
				CoordLog().Warningf("namespace %v isr node %v is lost.", nsInfo.GetDesp(), replica)
				needMigrate = true
				checkOK = false
			} else {
				aliveCount++
			}
		}
		if currentNodesEpoch != atomic.LoadInt64(&self.nodesEpoch) {
			CoordLog().Infof("nodes changed while checking namespaces: %v, %v", currentNodesEpoch, atomic.LoadInt64(&self.nodesEpoch))
			atomic.StoreInt32(&self.isClusterUnstable, 1)
			return
		}
		// should copy to avoid reused
		namespaceInfo := nsInfo

		partitions, ok := waitingMigrateNamespace[nsInfo.Name]
		if !ok {
			partitions = make(map[int]time.Time)
			waitingMigrateNamespace[nsInfo.Name] = partitions
		}
		if needMigrate {
			if _, ok := partitions[nsInfo.Partition]; !ok {
				partitions[nsInfo.Partition] = time.Now()
				// migrate next time
				continue
			}

			if atomic.LoadInt32(&self.isUpgrading) == 1 {
				CoordLog().Infof("wait checking namespaces since the cluster is upgrading")
				continue
			}
			failedTime := partitions[nsInfo.Partition]
			emergency := (aliveCount <= nsInfo.Replica/2) && failedTime.Before(time.Now().Add(-1*waitEmergencyMigrateInterval))
			if emergency ||
				failedTime.Before(time.Now().Add(-1*waitMigrateInterval)) {
				aliveNodes, aliveEpoch := self.getCurrentNodesWithEpoch(nsInfo.Tags)
				if aliveEpoch != currentNodesEpoch {
					go self.triggerCheckNamespaces(nsInfo.Name, nsInfo.Partition, time.Second)
					continue
				}
				CoordLog().Infof("begin migrate the namespace :%v", nsInfo.GetDesp())
				self.handleNamespaceMigrate(&namespaceInfo, aliveNodes, aliveEpoch)
				delete(partitions, nsInfo.Partition)
				// migrate only one at once to reduce the migrate traffic
				atomic.StoreInt32(&self.isClusterUnstable, 1)
				for _, parts := range waitingMigrateNamespace {
					for pid, _ := range parts {
						parts[pid].Add(time.Second * 10)
					}
				}
				return
			} else {
				CoordLog().Infof("waiting migrate the namespace :%v since time: %v", nsInfo.GetDesp(), partitions[nsInfo.Partition])
			}
		} else {
			delete(partitions, nsInfo.Partition)
		}

		if aliveCount > nsInfo.Replica && atomic.LoadInt32(&self.balanceWaiting) == 0 {
			//remove the unwanted node in isr
			CoordLog().Infof("namespace %v isr %v is more than replicator: %v, %v", namespaceInfo.GetDesp(),
				namespaceInfo.RaftNodes, aliveCount, nsInfo.Replica)
			removeNode := self.dpm.decideUnwantedRaftNode(&namespaceInfo, currentNodes)
			if removeNode != "" {
				coordErr := self.removeNamespaceFromNode(&namespaceInfo, removeNode)
				if coordErr == nil {
					CoordLog().Infof("node %v removed by plan from namespace : %v", removeNode, nsInfo)
				}
			}
		}
	}
	if checkOK {
		if fullCheck {
			atomic.StoreInt32(&self.isClusterUnstable, 0)
		}
	} else {
		atomic.StoreInt32(&self.isClusterUnstable, 1)
	}
}

func (self *PDCoordinator) handleNamespaceMigrate(nsInfo *PartitionMetaInfo,
	currentNodes map[string]NodeInfo, currentNodesEpoch int64) {
	if currentNodesEpoch != atomic.LoadInt64(&self.nodesEpoch) {
		return
	}
	isrChanged := false
	aliveReplicas := 0
	failedList := make([]string, 0)
	for _, replica := range nsInfo.RaftNodes {
		if _, ok := currentNodes[replica]; ok {
			aliveReplicas++
		} else {
			failedList = append(failedList, replica)
		}
	}

	for i := aliveReplicas; i < nsInfo.Replica; i++ {
		n, err := self.dpm.allocNodeForNamespace(nsInfo, currentNodes)
		if err != nil {
			CoordLog().Infof("failed to get a new raft node for namespace: %v", nsInfo.GetDesp())
		} else {
			nsInfo.MaxRaftID++
			nsInfo.RaftIDs[n.GetID()] = uint64(nsInfo.MaxRaftID)
			nsInfo.RaftNodes = append(nsInfo.RaftNodes, n.GetID())
			isrChanged = true
		}
	}
	if len(nsInfo.RaftNodes) > nsInfo.Replica {
		failedList = failedList[:len(nsInfo.RaftNodes)-nsInfo.Replica]
		nsInfo.RaftNodes = FilterList(nsInfo.RaftNodes, failedList)
		CoordLog().Infof("remove failed raft node %v for namespace: %v", failedList, nsInfo.GetDesp())
		for _, n := range failedList {
			delete(nsInfo.RaftIDs, n)
			CoordLog().Infof("remove failed raft node %v for namespace: %v", n, nsInfo.GetDesp())
			isrChanged = true
		}
	}

	if isrChanged {
		err := self.register.UpdateNamespacePartReplicaInfo(nsInfo.Name, nsInfo.Partition,
			&nsInfo.PartitionReplicaInfo, nsInfo.PartitionReplicaInfo.Epoch)
		if err != nil {
			CoordLog().Infof("update namespace replica info failed: %v", err.Error())
			return
		} else {
			CoordLog().Infof("namespace %v migrate to replicas : %v", nsInfo.GetDesp(), nsInfo.RaftNodes)
		}
	}
}

func (self *PDCoordinator) addNamespaceToNode(nsInfo *PartitionMetaInfo, nid string) *CoordErr {
	nsInfo.RaftNodes = append(nsInfo.RaftNodes, nid)
	if !self.dpm.checkNamespaceNodeConflict(nsInfo) {
		return ErrNamespaceNodeConflict
	}
	nsInfo.MaxRaftID++
	nsInfo.RaftIDs[nid] = uint64(nsInfo.MaxRaftID)

	err := self.register.UpdateNamespacePartReplicaInfo(nsInfo.Name, nsInfo.Partition,
		&nsInfo.PartitionReplicaInfo, nsInfo.PartitionReplicaInfo.Epoch)
	if err != nil {
		CoordLog().Infof("add namespace replica info failed: %v", err.Error())
		return &CoordErr{err.Error(), RpcNoErr, CoordRegisterErr}
	} else {
		CoordLog().Infof("namespace %v replica : %v added", nsInfo.GetDesp(), nid)
	}
	return nil
}

func (self *PDCoordinator) removeNamespaceFromNode(nsInfo *PartitionMetaInfo, nid string) *CoordErr {
	nodes := make([]string, 0, len(nsInfo.RaftNodes))
	for _, replica := range nsInfo.RaftNodes {
		if nid == replica {
			continue
		}
		nodes = append(nodes, replica)
	}
	if len(nodes) < 1 {
		CoordLog().Infof("single replica can not be removed from namespace %v ", nsInfo.GetDesp())
		return ErrNamespaceReplicaNotEnough
	}
	nsInfo.RaftNodes = nodes
	delete(nsInfo.RaftIDs, nid)
	err := self.register.UpdateNamespacePartReplicaInfo(nsInfo.Name, nsInfo.Partition,
		&nsInfo.PartitionReplicaInfo, nsInfo.PartitionReplicaInfo.Epoch)
	if err != nil {
		CoordLog().Infof("update namespace replica info failed: %v", err.Error())
		return &CoordErr{err.Error(), RpcNoErr, CoordRegisterErr}
	} else {
		CoordLog().Infof("namespace %v replica removed from node:%v", nsInfo.GetDesp(), nid)
	}
	return nil
}