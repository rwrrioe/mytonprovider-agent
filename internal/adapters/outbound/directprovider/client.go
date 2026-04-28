package directprovider

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/big"
	"math/rand"
	"strconv"
	"time"

	"github.com/xssnick/tonutils-go/adnl"
	"github.com/xssnick/tonutils-go/adnl/keys"
	"github.com/xssnick/tonutils-go/adnl/overlay"
	"github.com/xssnick/tonutils-go/adnl/rldp"
	"github.com/xssnick/tonutils-go/tl"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"github.com/xssnick/tonutils-storage-provider/pkg/transport"
	"github.com/xssnick/tonutils-storage/storage"

	"github.com/rwrrioe/mytonprovider-agent/internal/domain"
	"github.com/rwrrioe/mytonprovider-agent/internal/lib/sl"
)

const (
	probeFakeSize  = 1
	pingTimeout    = 5 * time.Second
	rlQueryTimeout = 10 * time.Second
)

// Client реализует poll.RatesProbe и proof.BagProver.
//
// RatesProbe идёт через transport.Client (свой ADNL-канал к storage-provider контракту).
// BagProver идёт напрямую к storage-узлу провайдера через adnl.Gateway → RLDP.
type Client struct {
	transport *transport.Client
	gateway   *adnl.Gateway
	logger    *slog.Logger
}

func New(transport *transport.Client, gateway *adnl.Gateway, logger *slog.Logger) *Client {
	return &Client{
		transport: transport,
		gateway:   gateway,
		logger:    logger,
	}
}

// Probe запрашивает rates у провайдера через ADNL.
// Если провайдер недоступен или не объявил себя available — online=false.
// Соответствует бэкенд-вызову providerClient.GetStorageRates(ctx, pubkey, fakeSize=1).
func (c *Client) Probe(
	ctx context.Context,
	pubkey []byte,
) (rates domain.Rates, online bool, err error) {
	const op = "adapters.directprovider.Probe"

	res, err := c.transport.GetStorageRates(ctx, pubkey, probeFakeSize)
	if err != nil {
		return domain.Rates{}, false, fmt.Errorf("%s:%w", op, err)
	}
	if res == nil || !res.Available {
		return domain.Rates{}, false, nil
	}

	rates = domain.Rates{
		RatePerMBDay: new(big.Int).SetBytes(res.RatePerMBDay).Int64(),
		MinBounty:    new(big.Int).SetBytes(res.MinBounty).Int64(),
		MinSpan:      res.MinSpan,
		MaxSpan:      res.MaxSpan,
	}
	return rates, true, nil
}

// Verify проверяет один bag у провайдера: загружает torrent info, рандомный piece
// и валидирует Merkle-proof. Возвращает domain.ReasonCode (включая ValidStorageProof
// при успехе).
// Соответствует бэкенд-функции checkPiece.
func (c *Client) Verify(
	ctx context.Context,
	ep domain.ProviderEndpoint,
	bagID string,
) (domain.ReasonCode, error) {
	const op = "adapters.directprovider.Verify"

	log := c.logger.With(
		slog.String("op", op),
		slog.String("provider_pubkey", ep.PublicKey),
		slog.String("bag_id", bagID),
	)

	if ep.Storage.IP == "" || ep.Storage.Port == 0 || len(ep.Storage.PublicKey) == 0 {
		return domain.IPNotFound, nil
	}

	addr := ep.Storage.IP + ":" + strconv.Itoa(int(ep.Storage.Port))

	peer, err := c.gateway.RegisterClient(addr, ep.Storage.PublicKey)
	if err != nil {
		log.Debug("failed to create ADNL peer", sl.Err(err))
		return domain.CantCreatePeer, nil
	}

	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	_, err = peer.Ping(pingCtx)
	cancel()
	if err != nil {
		log.Debug("initial ping failed", sl.Err(err))
		return domain.FailedInitialPing, nil
	}

	rl := rldp.NewClientV2(peer)
	defer rl.Close()

	return checkPiece(ctx, rl, bagID, log), nil
}

// checkPiece повторяет логику бэкенда (providersMaster/worker.go: checkPiece).
func checkPiece(
	ctx context.Context,
	rl *rldp.RLDP,
	bagID string,
	log *slog.Logger,
) domain.ReasonCode {
	peer, ok := rl.GetADNL().(adnl.Peer)
	if !ok {
		log.Error("failed to get ADNL peer")
		return domain.UnknownPeer
	}

	peer.Reinit()
	est := time.Now()

	pingCtx, pc := context.WithTimeout(ctx, pingTimeout)
	_, err := peer.Ping(pingCtx)
	pc()
	if err != nil {
		log.Debug("ping failed", sl.Err(err))
		return domain.PingFailed
	}

	bag, err := hex.DecodeString(bagID)
	if err != nil {
		log.Error("invalid bag id", sl.Err(err))
		return domain.InvalidBagID
	}

	over, err := tl.Hash(keys.PublicKeyOverlay{Key: bag})
	if err != nil {
		log.Debug("failed to hash overlay key", sl.Err(err))
		return domain.InvalidBagID
	}

	if time.Since(est) > 5*time.Second {
		peer.Reinit()
		est = time.Now()
	}

	var info storage.TorrentInfoContainer
	rlCtx, rlc := context.WithTimeout(ctx, rlQueryTimeout)
	err = rl.DoQuery(rlCtx, 32<<20, overlay.WrapQuery(over, &storage.GetTorrentInfo{}), &info)
	rlc()
	if err != nil {
		log.Debug("failed to get torrent info", sl.Err(err))
		return domain.GetInfoFailed
	}

	cl, err := cell.FromBOC(info.Data)
	if err != nil {
		log.Debug("failed to parse torrent info BoC", sl.Err(err))
		return domain.InvalidHeader
	}

	if !bytes.Equal(cl.Hash(), bag) {
		log.Debug("torrent info hash mismatch")
		return domain.InvalidHeader
	}

	var torrent storage.TorrentInfo
	if err = tlb.LoadFromCell(&torrent, cl.BeginParse()); err != nil {
		log.Debug("failed to load torrent info from cell", sl.Err(err))
		return domain.InvalidHeader
	}

	pieceID := int32(1)
	var totalPieces int32
	if torrent.PieceSize != 0 {
		totalPieces = int32(torrent.FileSize / uint64(torrent.PieceSize))
	}
	if totalPieces > 0 {
		pieceID = rand.Int31n(totalPieces)
	}

	if time.Since(est) > 5*time.Second {
		peer.Reinit()
	}

	var piece storage.Piece
	rl2Ctx, rl2c := context.WithTimeout(ctx, rlQueryTimeout)
	err = rl.DoQuery(rl2Ctx, 32<<20, overlay.WrapQuery(over, &storage.GetPiece{PieceID: pieceID}), &piece)
	rl2c()
	if err != nil {
		log.Debug("failed to get piece", sl.Err(err))
		return domain.CantGetPiece
	}

	proof, err := cell.FromBOC(piece.Proof)
	if err != nil {
		log.Debug("failed to parse piece proof BoC", sl.Err(err))
		return domain.CantParseBoC
	}

	if err = cell.CheckProof(proof, torrent.RootHash); err != nil {
		log.Debug("merkle proof check failed", sl.Err(err))
		return domain.ProofCheckFailed
	}

	return domain.ValidStorageProof
}
