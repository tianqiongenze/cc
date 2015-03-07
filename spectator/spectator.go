package spectator

import (
	"errors"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jxwr/cc/redis"
	"github.com/jxwr/cc/topo"
)

var (
	ErrNoSeed           = errors.New("spectator: no seed node found")
	ErrInvalidTag       = errors.New("spectator: invalid tag")
	ErrNodeNotExist     = errors.New("spectator: node not exist")
	ErrNodesInfoNotSame = errors.New("spectator: 'cluster nodes' info returned by seeds are different")
)

type Spectator struct {
	mutex       *sync.RWMutex
	Seeds       []*topo.Node
	ClusterTopo *topo.Cluster
}

func NewSpectator(seeds []*topo.Node) *Spectator {
	sp := &Spectator{
		mutex: &sync.RWMutex{},
		Seeds: seeds,
	}
	return sp
}

func (self *Spectator) Run() {
	tickChan := time.NewTicker(time.Second * 1).C
	for {
		select {
		case <-tickChan:
			self.BuildClusterTopo()
		}
	}
}

type reploff struct {
	NodeId string
	Offset int64
}

// 失败返回-1
func fetchReplOffset(addr string) int64 {
	info, err := redis.FetchInfo(addr, "Replication")
	if err != nil {
		return -1
	}
	if info.Get("role") == "master" {
		offset, err := info.GetInt64("master_repl_offset")
		if err != nil {
			return -1
		} else {
			return offset
		}
	}
	offset, err := info.GetInt64("slave_repl_offset")
	if err != nil {
		return -1
	}
	return offset
}

// 获取分片内ReplOffset最大的节点
func (self *Spectator) MaxReploffSlibing(nodeId string, slaveOnly bool) (string, error) {
	self.mutex.RLock()
	defer self.mutex.RUnlock()

	rs := self.ClusterTopo.FindReplicaSetByNode(nodeId)
	if rs == nil {
		return "", ErrNodeNotExist
	}

	rmap := self.FetchReplOffsetInReplicaSet(rs)

	var maxVal int64 = -1
	maxId := ""
	for id, val := range rmap {
		node := self.ClusterTopo.FindNode(id)
		if slaveOnly && node.IsMaster() {
			continue
		}
		if val > maxVal {
			maxVal = val
			maxId = id
		}
	}

	return maxId, nil
}

func (self *Spectator) FetchReplOffsetInReplicaSet(rs *topo.ReplicaSet) map[string]int64 {
	self.mutex.RLock()
	defer self.mutex.RUnlock()

	nodes := rs.AllNodes()
	c := make(chan reploff, len(nodes))

	for _, node := range nodes {
		go func(id, addr string) {
			offset := fetchReplOffset(addr)
			c <- reploff{id, offset}
		}(node.Id(), node.Addr())
	}

	result := map[string]int64{}
	for i := 0; i < len(nodes); i++ {
		off := <-c
		result[off.NodeId] = off.Offset
	}
	return result
}

func (self *Spectator) buildNode(line string) (*topo.Node, error) {
	xs := strings.Split(line, " ")
	mod, tag, id, addr, flags, parent := xs[0], xs[1], xs[2], xs[3], xs[4], xs[5]
	node := topo.NewNodeFromString(addr)
	ranges := []string{}
	for _, word := range xs[10:] {
		if strings.HasPrefix(word, "[") {
			node.SetMigrating(true)
			continue
		}
		ranges = append(ranges, word)
	}
	sort.Strings(ranges)

	for _, r := range ranges {
		xs = strings.Split(r, "-")
		if len(xs) == 2 {
			left, _ := strconv.Atoi(xs[0])
			right, _ := strconv.Atoi(xs[1])
			node.AddRange(topo.Range{left, right})
		}
	}

	// basic info
	node.SetId(id)
	node.SetParentId(parent)
	node.SetTag(tag)
	node.SetReadable(mod[0] == 'r')
	node.SetWritable(mod[1] == 'w')
	if strings.Contains(flags, "master") {
		node.SetRole("master")
	} else {
		node.SetRole("slave")
	}
	if strings.Contains(flags, "fail?") {
		node.SetPFail(true)
		node.IncrPFailCount()
	}
	xs = strings.Split(tag, ":")
	if len(xs) != 3 {
		return nil, ErrInvalidTag
	}
	node.SetRegion(xs[0])
	node.SetZone(xs[1])
	node.SetRoom(xs[2])

	return node, nil
}

func (self *Spectator) initClusterTopo(seed *topo.Node) (*topo.Cluster, error) {
	resp, err := redis.ClusterNodes(seed.Addr())
	if err != nil {
		return nil, err
	}

	cluster := topo.NewCluster("bj")

	lines := strings.Split(resp, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		node, err := self.buildNode(line)
		if err != nil {
			return nil, err
		}
		cluster.AddNode(node)
	}

	return cluster, nil
}

func (self *Spectator) checkClusterTopo(seed *topo.Node, cluster *topo.Cluster) error {
	resp, err := redis.ClusterNodes(seed.Addr())
	if err != nil {
		return err
	}

	lines := strings.Split(resp, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		s, err := self.buildNode(line)
		if err != nil {
			return err
		}

		node := cluster.FindNode(s.Id())
		if node == nil {
			return ErrNodeNotExist
		}

		if !node.Compare(s) {
			return ErrNodesInfoNotSame
		}

		if s.PFail() {
			node.IncrPFailCount()
		}
	}

	return nil
}

func (self *Spectator) BuildClusterTopo() (*topo.Cluster, error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if len(self.Seeds) == 0 {
		return nil, ErrNoSeed
	}

	seeds := []*topo.Node{}
	for _, s := range self.Seeds {
		if redis.IsAlive(s.Addr()) {
			seeds = append(seeds, s)
		}
	}

	if len(seeds) == 0 {
		return nil, ErrNoSeed
	}

	seed := seeds[0]
	cluster, err := self.initClusterTopo(seed)
	if err != nil {
		return nil, err
	}

	if len(seeds) > 1 {
		for _, seed := range seeds[1:] {
			err := self.checkClusterTopo(seed, cluster)
			if err != nil {
				return nil, err
			}
		}
	}

	for _, s := range cluster.LocalRegionNodes() {
		if s.PFailCount() > cluster.NumLocalRegionNode()/2 {
			log.Printf("found %d/%d PFAIL state on %s, turning into FAIL state.",
				s.PFailCount(), cluster.NumLocalRegionNode(), s.Addr())
			s.SetFail(true)
		}
	}

	cluster.BuildReplicaSets()

	self.Seeds = cluster.LocalRegionNodes()
	self.ClusterTopo = cluster

	return cluster, nil
}