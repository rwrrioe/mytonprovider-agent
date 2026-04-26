package adnlclients

import (
	"context"
	"crypto/ed25519"
	"fmt"

	"github.com/xssnick/tonutils-go/adnl"
	"github.com/xssnick/tonutils-go/adnl/dht"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-storage-provider/pkg/transport"
)

type Clients struct {
	DHT       *dht.Client
	Transport *transport.Client
}

func New(
	ctx context.Context,
	configURL string,
	ADNLPort string,
	privateKey ed25519.PrivateKey,
) (clients Clients, err error) {
	lsCfg, err := liteclient.GetConfigFromUrl(ctx, configURL)
	if err != nil {
		err = fmt.Errorf("failed to get liteclient config: %w", err)
		return
	}

	_, dhtAdnlKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		err = fmt.Errorf("failed to generate DHT ADNL key: %w", err)
		return
	}

	dl, err := adnl.DefaultListener("0.0.0.0:" + ADNLPort)
	if err != nil {
		err = fmt.Errorf("failed to create default listener: %w", err)
		return
	}

	netMgr := adnl.NewMultiNetReader(dl)

	dhtGate := adnl.NewGatewayWithNetManager(dhtAdnlKey, netMgr)
	if err = dhtGate.StartClient(); err != nil {
		err = fmt.Errorf("failed to start DHT gateway: %w", err)
		return
	}

	clients.DHT, err = dht.NewClientFromConfig(dhtGate, lsCfg)
	if err != nil {
		err = fmt.Errorf("failed to create DHT client: %w", err)
		return
	}

	gateProvider := adnl.NewGatewayWithNetManager(privateKey, netMgr)
	if err = gateProvider.StartClient(); err != nil {
		err = fmt.Errorf("failed to start ADNL gateway for provider: %w", err)
		return
	}

	clients.Transport = transport.NewClient(gateProvider, clients.DHT)

	return
}
