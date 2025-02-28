package database

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/lindb/lindb/constants"
	"github.com/lindb/lindb/coordinator/discovery"
	"github.com/lindb/lindb/coordinator/storage"
	"github.com/lindb/lindb/models"
	"github.com/lindb/lindb/pkg/logger"
	"github.com/lindb/lindb/pkg/state"
)

//go:generate mockgen -source=./admin_state_machine.go -destination=./admin_state_machine_mock.go -package=database

// AdminStateMachine is database config controller,
// creates shard assignment based on config and active nodes related storage cluster.
// runtime watches database change event, maintain shard assignment and create related coordinator task.
type AdminStateMachine interface {
	discovery.Listener

	// Close closes admin state machine, stops watch change event
	Close() error
}

// adminStateMachine implement admin state machine interface.
// all metadata change will store related storage cluster.
type adminStateMachine struct {
	storageCluster storage.ClusterStateMachine
	discovery      discovery.Discovery

	mutex  sync.RWMutex
	ctx    context.Context
	cancel context.CancelFunc

	log *logger.Logger
}

// NewAdminStateMachine creates admin state machine instance
func NewAdminStateMachine(ctx context.Context, discoveryFactory discovery.Factory,
	storageCluster storage.ClusterStateMachine) (AdminStateMachine, error) {
	c, cancel := context.WithCancel(ctx)
	// new admin state machine instance
	stateMachine := &adminStateMachine{
		storageCluster: storageCluster,
		ctx:            c,
		cancel:         cancel,
		log:            logger.GetLogger("database/admin/state/machine"),
	}
	// new database config discovery
	stateMachine.discovery = discoveryFactory.CreateDiscovery(constants.DatabaseConfigPath, stateMachine)
	if err := stateMachine.discovery.Discovery(); err != nil {
		return nil, fmt.Errorf("discovery database config error:%s", err)
	}
	return stateMachine, nil
}

// OnCreate creates shard assignment when receive database create event
func (sm *adminStateMachine) OnCreate(key string, resource []byte) {
	cfg := models.Database{}
	if err := json.Unmarshal(resource, &cfg); err != nil {
		sm.log.Error("discovery database create but unmarshal error",
			logger.String("data", string(resource)), logger.Error(err))
		return
	}

	if len(cfg.Name) == 0 {
		sm.log.Error("database name cannot be empty", logger.String("data", string(resource)))
		return
	}

	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	cluster := sm.storageCluster.GetCluster(cfg.Cluster)
	if cluster == nil {
		sm.log.Error("storage cluster not exist",
			logger.String("cluster", cfg.Cluster))
		return
	}
	shardAssign, err := cluster.GetShardAssign(cfg.Name)
	if err != nil && err != state.ErrNotExist {
		sm.log.Error("get shard assign error", logger.Error(err))
		return
	}
	// build shard assignment for creation database, generate related coordinator task
	if shardAssign == nil {
		if err := sm.createShardAssignment(cfg.Name, cluster, &cfg); err != nil {
			sm.log.Error("create shard assignment error",
				logger.String("data", string(resource)), logger.Error(err))
		}
	}

	//} else if len(shardAssign.Shards) != cfg.NumOfShard {
	//TODO need implement modify database shard num.
}

func (sm *adminStateMachine) OnDelete(key string) {
	//TODO impl delete database???
	//panic("implement me")
}

// Close closes admin state machine, stops watch change event
func (sm *adminStateMachine) Close() error {
	sm.discovery.Close()
	sm.cancel()
	return nil
}

// createShardAssignment creates shard assignment for spec cluster
// 1) generate shard assignment
// 2) save shard assignment into related storage cluster
// 3) submit create shard coordinator task(storage node will execute it when receive task event)
func (sm *adminStateMachine) createShardAssignment(databaseName string,
	cluster storage.Cluster, cfg *models.Database) error {
	activeNodes := cluster.GetActiveNodes()
	if len(activeNodes) == 0 {
		return fmt.Errorf("active node not found")
	}
	//TODO need calc resource and pick related node for store data
	var nodes = make(map[int]*models.Node)
	for idx, node := range activeNodes {
		nodes[idx] = &node.Node
	}

	var nodeIDs []int
	for idx := range nodes {
		nodeIDs = append(nodeIDs, idx)
	}

	// generate shard assignment based on node ids and config
	shardAssign, err := ShardAssignment(nodeIDs, cfg)
	if err != nil {
		return err
	}
	// set nodes and config, storage node will use it when execute create shard task
	shardAssign.Nodes = nodes

	// save shard assignment into related storage cluster
	if err := cluster.SaveShardAssign(databaseName, shardAssign, cfg.Engine); err != nil {
		return err
	}
	return nil
}
