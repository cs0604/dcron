package etcd

import (
	"context"
	"github.com/google/uuid"
	"github.com/libi/dcron/driver"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/client/v3"
	"log"
	"sync"
	"time"
)

var _ driver.Driver = &EtcdDriver{}

const (
	defaultLease    = 5 // 5 second ttl
	dialTimeout     = 3 * time.Second
	businessTimeout = 5 * time.Second
)

type EtcdDriver struct {
	cli        *clientv3.Client
	lease      int64
	serverList map[string]map[string]string
	lock       sync.Mutex
}

//NewEtcdDriver ...
func NewEtcdDriver(config *clientv3.Config) (*EtcdDriver, error) {
	cli, err := clientv3.New(*config)
	if err != nil {
		return nil, err
	}

	ser := &EtcdDriver{
		cli:        cli,
		serverList: make(map[string]map[string]string, 10),
	}

	return ser, nil
}

//设置key value，绑定租约
func (s *EtcdDriver) putKeyWithLease(key, val string) (clientv3.LeaseID, error) {
	//设置租约时间，最少5s
	if s.lease < defaultLease {
		s.lease = defaultLease
	}

	ctx, cancel := context.WithTimeout(context.Background(), businessTimeout)
	defer cancel()

	resp, err := s.cli.Grant(ctx, s.lease)
	if err != nil {
		return 0, err
	}
	leaseID := resp.ID
	//注册服务并绑定租约
	_, err = s.cli.Put(ctx, key, val, clientv3.WithLease(leaseID))
	if err != nil {
		return 0, err
	}

	return leaseID, nil
}

func (s *EtcdDriver) randNodeID(serviceName string) (nodeID string) {
	return getPrefix(serviceName) + uuid.New().String()
}

//WatchService 初始化服务列表和监视
func (s *EtcdDriver) watchService(serviceName string) error {
	prefix := getPrefix(serviceName)
	//根据前缀获取现有的key
	resp, err := s.cli.Get(context.Background(), prefix, clientv3.WithPrefix())
	if err != nil {
		return err
	}

	for _, ev := range resp.Kvs {
		s.setServiceList(serviceName, string(ev.Key), string(ev.Value))
	}

	//监视前缀，修改变更的server
	go s.watcher(serviceName)
	return nil
}

func getPrefix(serviceName string) string {
	return serviceName + "/"
}

//watcher 监听前缀
func (s *EtcdDriver) watcher(serviceName string) {
	prefix := getPrefix(serviceName)
	rch := s.cli.Watch(context.Background(), prefix, clientv3.WithPrefix())
	for wresp := range rch {
		for _, ev := range wresp.Events {
			switch ev.Type {
			case mvccpb.PUT: //修改或者新增
				s.setServiceList(serviceName, string(ev.Kv.Key), string(ev.Kv.Value))
			case mvccpb.DELETE: //删除
				s.delServiceList(serviceName, string(ev.Kv.Key))
			}
		}
	}
}

//setServiceList 新增服务地址
func (s *EtcdDriver) setServiceList(serviceName, key, val string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	var nodeMap map[string]string
	var ok bool
	if nodeMap, ok = s.serverList[serviceName]; !ok {
		nodeMap = make(map[string]string)
		s.serverList[serviceName] = nodeMap
	}
	nodeMap[key] = val
}

//DelServiceList 删除服务地址
func (s *EtcdDriver) delServiceList(serviceName, key string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if nodeMap, ok := s.serverList[serviceName]; ok {
		delete(nodeMap, key)
	}
}

//GetServices 获取服务地址
func (s *EtcdDriver) getServices(serviceName string) []string {
	s.lock.Lock()
	defer s.lock.Unlock()
	addrs := make([]string, 0)
	if nodeMap, ok := s.serverList[serviceName]; ok {
		for _, v := range nodeMap {
			addrs = append(addrs, v)
		}
	}
	return addrs
}

func (e *EtcdDriver) Ping() error {
	return nil
}

func (e *EtcdDriver) SetHeartBeat(nodeID string) {

	leaseID, err := e.putKeyWithLease(nodeID, nodeID)
	if err != nil {
		log.Printf("putKeyWithLease error: %v", err)
		return
	}

	//设置续租  此处ctx不能设置超时
	leaseRespChan, err := e.cli.KeepAlive(context.Background(), leaseID)

	if err != nil {
		log.Printf("keepalive error:%v", err)
		return
	}

	go func() {
		for {
			_ = <-leaseRespChan
			//log.Printf("续约成功 %v", resp)
		}
		//log.Printf("关闭续租")
	}()

}

func (e *EtcdDriver) SetTimeout(timeout time.Duration) {

	e.lease = int64(timeout.Seconds())
}

func (e *EtcdDriver) GetServiceNodeList(serviceName string) ([]string, error) {
	return e.getServices(serviceName), nil
}

func (e *EtcdDriver) RegisterServiceNode(serviceName string) (string, error) {
	nodeId := e.randNodeID(serviceName)
	_, err := e.putKeyWithLease(nodeId, nodeId)
	if err != nil {
		return "", err
	}
	err = e.watchService(serviceName)
	if err != nil {
		return "", err
	}
	return nodeId, nil
}
