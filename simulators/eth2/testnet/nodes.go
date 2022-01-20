package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/hive/hivesim"
	"github.com/protolambda/eth2api"
	"github.com/protolambda/eth2api/client/nodeapi"
)

const (
	PortUserRPC      = 8545
	PortEngineRPC    = 8545 // TO BE CHANGED SOON
	PortBeaconTCP    = 9000
	PortBeaconUDP    = 9000
	PortBeaconAPI    = 4000
	PortBeaconGRPC   = 4001
	PortMetrics      = 8080
	PortValidatorAPI = 5000
)

// TODO: we assume the clients were configured with default ports.
// Would be cleaner to run a script in the client to get the address without assumptions

type Eth1Node struct {
	*hivesim.Client
}

func (en *Eth1Node) EnodeURLNetwork(sim *hivesim.Simulation, t *hivesim.T, network string) (string, error) {
	originalEnode, err := en.EnodeURL()
	if err != nil {
		return "", err
	}
	n, err := enode.ParseV4(originalEnode)
	if err != nil {
		return "", err
	}
	netIP, err := sim.ContainerNetworkIP(t.SuiteID, network, en.Container)
	if err != nil {
		return "", err
	}
	// Check ports returned
	tcpPort := n.TCP()
	if tcpPort == 0 {
		tcpPort = 30303
	}
	udpPort := n.UDP()
	if udpPort == 0 {
		udpPort = 30303
	}
	fixedIP := enode.NewV4(n.Pubkey(), net.ParseIP(netIP), tcpPort, udpPort)
	return fixedIP.URLv4(), nil
}

func (en *Eth1Node) NetworkIP(sim *hivesim.Simulation, t *hivesim.T, network string) (string, error) {
	netIP, err := sim.ContainerNetworkIP(t.SuiteID, network, en.Container)
	if err != nil {
		return "", err
	}
	return netIP, nil
}

func (en *Eth1Node) UserRPCAddress() (string, error) {
	return fmt.Sprintf("http://%v:%d", en.IP, PortUserRPC), nil
}

func (en *Eth1Node) UserRPCAddressWithIP(ip string) (string, error) {
	return fmt.Sprintf("http://%v:%d", ip, PortUserRPC), nil
}

func (en *Eth1Node) EngineRPCAddress() (string, error) {
	// TODO what will the default port be?
	return fmt.Sprintf("http://%v:%d", en.IP, PortEngineRPC), nil
}

type BeaconNode struct {
	*hivesim.Client
	API *eth2api.Eth2HttpClient
}

func NewBeaconNode(cl *hivesim.Client) *BeaconNode {
	return &BeaconNode{
		Client: cl,
		API: &eth2api.Eth2HttpClient{
			Addr:  fmt.Sprintf("http://%s:%d", cl.IP, PortBeaconAPI),
			Cli:   &http.Client{},
			Codec: eth2api.JSONCodec{},
		},
	}
}

func (bn *BeaconNode) ENR() (string, error) {
	ctx, _ := context.WithTimeout(context.Background(), time.Second*10)
	var out eth2api.NetworkIdentity
	if err := nodeapi.Identity(ctx, bn.API, &out); err != nil {
		return "", err
	}
	fmt.Printf("[%v] p2p addrs: %v\n", bn.Container, out.P2PAddresses)
	fmt.Printf("[%v] peer id: %s\n", bn.Container, out.PeerID)
	fmt.Printf("[%v] enr: %s\n", bn.Container, out.ENR)
	return out.ENR, nil
}

func (bn *BeaconNode) EnodeURL() (string, error) {
	return "", errors.New("beacon node does not have an discv4 Enode URL, use ENR or multi-address instead")
}

type ValidatorClient struct {
	*hivesim.Client
}
