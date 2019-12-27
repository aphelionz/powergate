package api

import (
	"context"
	"io"

	"github.com/ipfs/go-cid"
	pb "github.com/textileio/filecoin/api/pb"
	"github.com/textileio/filecoin/deals"
	"github.com/textileio/filecoin/lotus/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type service struct {
	pb.UnimplementedAPIServer

	dealModule *deals.DealModule
}

type storeResult struct {
	Cids        []cid.Cid
	DealConfigs []deals.DealConfig
	Err         error
}

func store(ctx context.Context, dealModule *deals.DealModule, storeParams *pb.StoreParams, reader io.Reader, ch chan storeResult) {
	defer close(ch)
	dealConfigs := make([]deals.DealConfig, len(storeParams.GetDealConfigs()))
	for i, dealConfig := range storeParams.GetDealConfigs() {
		dealConfigs[i] = deals.DealConfig{
			Miner:      dealConfig.GetMiner(),
			EpochPrice: types.NewInt(dealConfig.GetEpochPrice()),
		}
	}
	cids, dealConfigs, err := dealModule.Store(ctx, storeParams.GetAddress(), reader, dealConfigs, storeParams.GetDuration())
	if err != nil {
		ch <- storeResult{Err: err}
		return
	}
	ch <- storeResult{Cids: cids, DealConfigs: dealConfigs}
}

func (s *service) AvailableAsks(ctx context.Context, req *pb.AvailableAsksRequest) (*pb.AvailableAsksReply, error) {
	query := deals.Query{
		MaxPrice:  req.GetQuery().GetMaxPrice(),
		PieceSize: req.GetQuery().GetPieceSize(),
		Limit:     int(req.GetQuery().GetLimit()),
		Offset:    int(req.GetQuery().GetOffset()),
	}
	asks, err := s.dealModule.AvailableAsks(query)
	if err != nil {
		return nil, err
	}
	replyAsks := make([]*pb.StorageAsk, len(asks))
	for i, ask := range asks {
		replyAsks[i] = &pb.StorageAsk{
			Price:        ask.Price,
			MinPieceSize: ask.MinPieceSize,
			Miner:        ask.Miner,
			Timestamp:    ask.Timestamp,
			Expiry:       ask.Expiry,
		}
	}
	return &pb.AvailableAsksReply{Asks: replyAsks}, nil
}

func (s *service) Store(srv pb.API_StoreServer) error {
	req, err := srv.Recv()
	if err != nil {
		return err
	}
	var storeParams *pb.StoreParams
	switch payload := req.GetPayload().(type) {
	case *pb.StoreRequest_StoreParams:
		storeParams = payload.StoreParams
	default:
		return status.Errorf(codes.InvalidArgument, "expexted StoreParams for StoreRequest.Payload but got %T", payload)
	}

	reader, writer := io.Pipe()

	storeChannel := make(chan storeResult)
	go store(srv.Context(), s.dealModule, storeParams, reader, storeChannel)

	for {
		req, err := srv.Recv()
		if err == io.EOF {
			_ = writer.Close()
			break
		} else if err != nil {
			_ = writer.CloseWithError(err)
			break
		}
		switch payload := req.GetPayload().(type) {
		case *pb.StoreRequest_Chunk:
			_, writeErr := writer.Write(payload.Chunk)
			if writeErr != nil {
				return writeErr
			}
		default:
			return status.Errorf(codes.InvalidArgument, "expected Chunk for StoreRequest.Payload but got %T", payload)
		}
	}

	storeResult := <-storeChannel
	if storeResult.Err != nil {
		return storeResult.Err
	}

	replyCids := make([]string, len(storeResult.Cids))
	for i, cid := range storeResult.Cids {
		replyCids[i] = cid.String()
	}

	replyDealConfigs := make([]*pb.DealConfig, len(storeResult.DealConfigs))
	for i, dealConfig := range storeResult.DealConfigs {
		replyDealConfigs[i] = &pb.DealConfig{Miner: dealConfig.Miner, EpochPrice: dealConfig.EpochPrice.Uint64()}
	}

	return srv.SendAndClose(&pb.StoreReply{Cids: replyCids, DealConfigs: replyDealConfigs})
}

func (s *service) Watch(req *pb.WatchRequest, srv pb.API_WatchServer) error {
	proposals := make([]cid.Cid, len(req.GetProposals()))
	for i, proposal := range req.GetProposals() {
		id, err := cid.Decode(proposal)
		if err != nil {
			return err
		}
		proposals[i] = id
	}
	ch, err := s.dealModule.Watch(srv.Context(), proposals)
	if err != nil {
		return err
	}

	for {
		update, ok := <-ch
		if ok == false {
			break
		} else {
			dealInfo := &pb.DealInfo{
				ProposalCid:   update.ProposalCid.String(),
				StateID:       update.StateID,
				StateName:     update.StateName,
				Miner:         update.Miner,
				PieceRef:      update.PieceRef,
				Size:          update.Size,
				PricePerEpoch: update.PricePerEpoch.Uint64(),
				Duration:      update.Duration,
			}
			srv.Send(&pb.WatchReply{DealInfo: dealInfo})
		}
	}
	return nil
}
