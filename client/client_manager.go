package client

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/blackbeans/go-moa-client/client/hash"
	"github.com/blackbeans/go-moa/core"
	"github.com/blackbeans/go-moa/lb"
	"github.com/blackbeans/go-moa/proto"
	log "github.com/blackbeans/log4go"
	"github.com/blackbeans/turbo"
	tclient "github.com/blackbeans/turbo/client"
	"github.com/blackbeans/turbo/codec"
	"github.com/blackbeans/turbo/packet"
)

type MoaClientManager struct {
	clientsManager *tclient.ClientManager
	uri2Ips        map[string] /*uri*/ hash.Strategy
	addrManager    *AddressManager
	op             *ClientOption
	lock           sync.RWMutex
}

func NewMoaClientManager(op *ClientOption, uris []string) *MoaClientManager {
	var reg lb.IRegistry
	if strings.HasPrefix(op.RegistryHosts, core.SCHEMA_ZK) {
		reg = lb.NewZkRegistry(strings.TrimPrefix(op.RegistryHosts, core.SCHEMA_ZK), uris, false)
	}

	reconnect := tclient.NewReconnectManager(true, 10*time.Second, 10,
		func(ga *tclient.GroupAuth, remoteClient *tclient.RemotingClient) (bool, error) {
			return true, nil
		})

	manager := &MoaClientManager{}
	manager.op = op
	manager.clientsManager = tclient.NewClientManager(reconnect)
	manager.uri2Ips = make(map[string]hash.Strategy, 2)

	addrManager := NewAddressManager(reg, uris, manager.OnAddressChange)
	manager.addrManager = addrManager

	return manager
}

func (self MoaClientManager) OnAddressChange(uri string, hosts []string) {

	//新增地址
	addHostport := make([]string, 0, 2)

	self.lock.Lock()
	//寻找新增连接
	for _, ip := range hosts {
		exist := self.clientsManager.FindRemoteClient(ip)
		if nil == exist {
			addHostport = append(addHostport, ip)
		}
	}

	for _, hp := range addHostport {
		//		创建redis的实例
		addr, _ := net.ResolveTCPAddr("tcp", hp)
		conn, err := net.DialTCP("tcp", nil, addr)
		if nil != err {
			log.ErrorLog("config_center", "MoaClientManager|Create Client|FAIL|%s", hp)
			continue
		}

		//参数
		rcc := turbo.NewRemotingConfig(
			fmt.Sprintf("turbo-client:%s", hp),
			1000, 16*1024,
			16*1024, 20000, 20000,
			20*time.Second, 100000)

		c := tclient.NewRemotingClient(conn, func() codec.ICodec {
			return proto.BinaryCodec{
				MaxFrameLength: packet.MAX_PACKET_BYTES}
		}, func(c *tclient.RemotingClient, p *packet.Packet) {
			//转发给后端处理器
		}, rcc)
		c.Start()
		log.InfoLog("config_center", "MoaClientManager|Create Client|SUCC|%s", hp)
		self.clientsManager.Auth(tclient.NewGroupAuth(hp, ""), c)
	}

	if self.op.SelectorStrategy == hash.STRATEGY_KETAMA {
		self.uri2Ips[uri] = hash.NewKetamaStrategy(hosts)
	} else if self.op.SelectorStrategy == hash.STRATEGY_RANDOM {
		self.uri2Ips[uri] = hash.NewRandomStrategy(hosts)
	} else {
		self.uri2Ips[uri] = hash.NewRandomStrategy(hosts)
	}

	log.InfoLog("config_center", "MoaClientManager|Store Uri Pool|SUCC|%s|%v", uri, hosts)
	//清理掉不再使用redisClient
	usingIps := make(map[string]bool, 5)
	for _, v := range self.uri2Ips {
		v.Iterator(func(i int, ip string) {
			usingIps[ip] = true
		})
	}

	for ip := range self.clientsManager.ClientsClone() {
		_, ok := usingIps[ip]
		if !ok {
			//不再使用了移除
			self.clientsManager.DeleteClients(ip)
		}
	}
	self.lock.Unlock()

	log.InfoLog("config_center", "MoaClientManager|OnAddressChange|SUCC|%s|%v", uri, hosts)
}

//根据Uri获取连接
func (self MoaClientManager) SelectClient(uri string, key string) (*tclient.RemotingClient, error) {

	self.lock.RLock()
	defer self.lock.RUnlock()

	strategy, ok := self.uri2Ips[uri]
	if ok {
		ip := strategy.Select(key)
		p := self.clientsManager.FindRemoteClient(ip)
		if nil != p {
			return p, nil
		}
	}
	return nil, errors.New(fmt.Sprintf("NO CLIENT FOR %s", uri))

}

func (self MoaClientManager) Destroy() {
	self.clientsManager.Shutdown()
}
