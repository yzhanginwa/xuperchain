/*
 * Copyright (c) 2019. Baidu Inc. All Rights Reserved.
 */

package server

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"math/big"
	"net"
	"net/http"
	_ "net/http/pprof"
	"reflect"
	"strconv"

	"github.com/golang/protobuf/proto"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/xuperchain/log15"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/reflection"

	middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/xuperchain/xuperchain/core/common"
	xlog "github.com/xuperchain/xuperchain/core/common/log"
	"github.com/xuperchain/xuperchain/core/consensus"
	xchaincore "github.com/xuperchain/xuperchain/core/core"
	"github.com/xuperchain/xuperchain/core/global"
	p2p_base "github.com/xuperchain/xuperchain/core/p2p/base"
	xuper_p2p "github.com/xuperchain/xuperchain/core/p2p/pb"
	"github.com/xuperchain/xuperchain/core/pb"
	"github.com/xuperchain/xuperchain/core/server/xendorser"
)

type Server struct {
	log            log.Logger
	mg             *xchaincore.XChainMG
	dedupCache     *common.LRUCache
	dedupTimeLimit int
}

// PostTx Update db
func (s *Server) PostTx(ctx context.Context, in *pb.TxStatus) (*pb.CommonReply, error) {
	if in.Header == nil {
		in.Header = global.GHeader()
	}

	out, needRepost, err := s.mg.ProcessTx(in)
	if needRepost {
		go func() {
			msgInfo, _ := proto.Marshal(in)
			msg, _ := p2p_base.NewXuperMessage(p2p_base.XuperMsgVersion1, in.GetBcname(), in.GetHeader().GetLogid(), xuper_p2p.XuperMessage_POSTTX, msgInfo, xuper_p2p.XuperMessage_NONE)
			opts := []p2p_base.MessageOption{
				p2p_base.WithFilters([]p2p_base.FilterStrategy{p2p_base.DefaultStrategy}),
				p2p_base.WithBcName(in.GetBcname()),
				p2p_base.WithCompress(s.mg.GetXchainmgConfig().EnableCompress),
			}
			s.mg.P2pSvr.SendMessage(context.Background(), msg, opts...)
		}()
	}
	return out, err
}

// BatchPostTx batch update db
func (s *Server) BatchPostTx(ctx context.Context, in *pb.BatchTxs) (*pb.CommonReply, error) {
	if in.Header == nil {
		in.Header = global.GHeader()
	}
	out := &pb.CommonReply{Header: &pb.Header{Logid: in.Header.Logid}}
	succTxs := []*pb.TxStatus{}
	for _, v := range in.Txs {
		oneOut, needRepost, _ := s.mg.ProcessTx(v)
		if oneOut.Header.Error != pb.XChainErrorEnum_SUCCESS {
			if oneOut.Header.Error != pb.XChainErrorEnum_UTXOVM_ALREADY_UNCONFIRM_ERROR {
				s.log.Warn("BatchPostTx processTx error", "logid", in.Header.Logid, "error", oneOut.Header.Error, "txid", global.F(v.Txid))
			}
		} else if needRepost {
			succTxs = append(succTxs, v)
		}
	}
	in.Txs = succTxs //只广播成功的
	if len(in.Txs) > 0 {
		txsData, err := proto.Marshal(in)
		if err != nil {
			s.log.Error("handleBatchPostTx Marshal txs error", "error", err)
			return out, nil
		}

		msg, _ := p2p_base.NewXuperMessage(p2p_base.XuperMsgVersion1, "", in.GetHeader().GetLogid(), xuper_p2p.XuperMessage_BATCHPOSTTX, txsData, xuper_p2p.XuperMessage_NONE)
		opts := []p2p_base.MessageOption{
			p2p_base.WithFilters([]p2p_base.FilterStrategy{p2p_base.DefaultStrategy}),
			p2p_base.WithBcName(in.Txs[0].GetBcname()),
		}
		go s.mg.P2pSvr.SendMessage(context.Background(), msg, opts...)
	}
	return out, nil
}

// QueryContractStatData query statistic info about contract
func (s *Server) QueryContractStatData(ctx context.Context, in *pb.ContractStatDataRequest) (*pb.ContractStatDataResponse, error) {
	s.mg.Speed.Add("QueryContractStatData")
	if in.GetHeader() == nil {
		in.Header = global.GHeader()
	}
	out := &pb.ContractStatDataResponse{Header: &pb.Header{}}
	bc := s.mg.Get(in.GetBcname())

	if bc == nil {
		out.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE
		s.log.Trace("refuse a connection at function call QueryContractStatData", "logid", in.Header.Logid)
		return out, nil
	}
	contractStatDataResponse, contractStatDataErr := bc.QueryContractStatData()
	if contractStatDataErr != nil {
		return out, contractStatDataErr
	}

	return contractStatDataResponse, nil
}

// QueryUtxoRecord query utxo records
func (s *Server) QueryUtxoRecord(ctx context.Context, in *pb.UtxoRecordDetail) (*pb.UtxoRecordDetail, error) {
	s.mg.Speed.Add("QueryUtxoRecord")
	if in.GetHeader() == nil {
		in.Header = global.GHeader()
	}
	out := &pb.UtxoRecordDetail{Header: &pb.Header{}}
	bc := s.mg.Get(in.GetBcname())

	if bc == nil {
		out.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE
		s.log.Trace("refuse a connection at function call QueryUtxoRecord", "logid", in.Header.Logid)
		return out, nil
	}

	accountName := in.GetAccountName()
	if len(accountName) > 0 {
		utxoRecord, err := bc.QueryUtxoRecord(accountName, in.GetDisplayCount())
		if err != nil {
			return out, err
		}
		return utxoRecord, nil
	}

	return out, nil
}

// QueryAcl query some account info
func (s *Server) QueryACL(ctx context.Context, in *pb.AclStatus) (*pb.AclStatus, error) {
	s.mg.Speed.Add("QueryAcl")
	if in.Header == nil {
		in.Header = global.GHeader()
	}
	out := &pb.AclStatus{Header: &pb.Header{}}
	bc := s.mg.Get(in.Bcname)

	if bc == nil {
		out.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE // 拒绝
		s.log.Trace("refused a connection at function call QueryAcl", "logid", in.Header.Logid)
		return out, nil
	}

	accountName := in.GetAccountName()
	contractName := in.GetContractName()
	methodName := in.GetMethodName()
	if len(accountName) > 0 {
		acl, confirmed, err := bc.QueryAccountACL(accountName)
		out.Confirmed = confirmed
		if err != nil {
			return out, err
		}
		out.Acl = acl
		return out, nil
	} else if len(contractName) > 0 {
		if len(methodName) > 0 {
			acl, confirmed, err := bc.QueryContractMethodACL(contractName, methodName)
			out.Confirmed = confirmed
			if err != nil {
				return out, err
			}
			out.Acl = acl
			return out, nil
		}
	}
	return out, nil
}

// GetAccountContractsRequest get account request
func (s *Server) GetAccountContracts(ctx context.Context, in *pb.GetAccountContractsRequest) (*pb.GetAccountContractsResponse, error) {
	if in.Header == nil {
		in.Header = global.GHeader()
	}
	out := &pb.GetAccountContractsResponse{Header: &pb.Header{Logid: in.GetHeader().GetLogid()}}
	bc := s.mg.Get(in.GetBcname())
	if bc == nil {
		// bc not found
		out.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE
		s.log.Trace("refused a connection while GetAccountContracts", "logid", in.Header.Logid)
		return out, nil
	}
	contractsStatus, err := bc.GetAccountContractsStatus(in.GetAccount(), true)
	if err != nil {
		out.Header.Error = pb.XChainErrorEnum_ACCOUNT_CONTRACT_STATUS_ERROR
		s.log.Warn("GetAccountContracts error", "logid", in.Header.Logid, "error", err.Error())
		return out, err
	}
	out.ContractsStatus = contractsStatus
	return out, nil
}

// QueryTx Get transaction details
func (s *Server) QueryTx(ctx context.Context, in *pb.TxStatus) (*pb.TxStatus, error) {
	s.mg.Speed.Add("QueryTx")
	if in.Header == nil {
		in.Header = global.GHeader()
	}
	out := &pb.TxStatus{Header: &pb.Header{}}
	bc := s.mg.Get(in.Bcname)
	if bc == nil {
		out.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE // 拒绝
		return out, nil
	}
	if bc.QueryTxFromForbidden(in.Txid) {
		return out, errors.New("tx has been forbidden")
	}
	out = bc.QueryTx(in)
	return out, nil
}

// GetBalance get balance for account or addr
func (s *Server) GetBalance(ctx context.Context, in *pb.AddressStatus) (*pb.AddressStatus, error) {
	s.mg.Speed.Add("GetBalance")
	if in.Header == nil {
		in.Header = global.GHeader()
	}
	for i := 0; i < len(in.Bcs); i++ {
		bc := s.mg.Get(in.Bcs[i].Bcname)
		if bc == nil {
			in.Bcs[i].Error = pb.XChainErrorEnum_BLOCKCHAIN_NOTEXIST
			in.Bcs[i].Balance = ""
		} else {
			bi, err := bc.GetBalance(in.Address)
			if err != nil {
				in.Bcs[i].Error = HandleBlockCoreError(err)
				in.Bcs[i].Balance = ""
			} else {
				in.Bcs[i].Error = pb.XChainErrorEnum_SUCCESS
				in.Bcs[i].Balance = bi
			}
		}
	}
	return in, nil
}

// GetFrozenBalance get balance frozened for account or addr
func (s *Server) GetFrozenBalance(ctx context.Context, in *pb.AddressStatus) (*pb.AddressStatus, error) {
	s.mg.Speed.Add("GetFrozenBalance")
	if in.Header == nil {
		in.Header = global.GHeader()
	}
	for i := 0; i < len(in.Bcs); i++ {
		bc := s.mg.Get(in.Bcs[i].Bcname)
		if bc == nil {
			in.Bcs[i].Error = pb.XChainErrorEnum_BLOCKCHAIN_NOTEXIST
			in.Bcs[i].Balance = ""
		} else {
			bi, err := bc.GetFrozenBalance(in.Address)
			if err != nil {
				in.Bcs[i].Error = HandleBlockCoreError(err)
				in.Bcs[i].Balance = ""
			} else {
				in.Bcs[i].Error = pb.XChainErrorEnum_SUCCESS
				in.Bcs[i].Balance = bi
			}
		}
	}
	return in, nil
}

// GetFrozenBalance get balance frozened for account or addr
func (s *Server) GetBalanceDetail(ctx context.Context, in *pb.AddressBalanceStatus) (*pb.AddressBalanceStatus, error) {
	s.mg.Speed.Add("GetFrozenBalance")
	if in.Header == nil {
		in.Header = global.GHeader()
	}
	for i := 0; i < len(in.Tfds); i++ {
		bc := s.mg.Get(in.Tfds[i].Bcname)
		if bc == nil {
			in.Tfds[i].Error = pb.XChainErrorEnum_BLOCKCHAIN_NOTEXIST
			in.Tfds[i].Tfd = nil
		} else {
			tfd, err := bc.GetBalanceDetail(in.Address)
			if err != nil {
				in.Tfds[i].Error = HandleBlockCoreError(err)
				in.Tfds[i].Tfd = nil
			} else {
				in.Tfds[i].Error = pb.XChainErrorEnum_SUCCESS
				//				in.Bcs[i].Balance = bi
				in.Tfds[i] = tfd
			}
		}
	}
	return in, nil
}

// GetBlock get block info according to blockID
func (s *Server) GetBlock(ctx context.Context, in *pb.BlockID) (*pb.Block, error) {
	s.mg.Speed.Add("GetBlock")
	if in.Header == nil {
		in.Header = global.GHeader()
	}
	s.log.Trace("Start to dealwith GetBlock", "logid", in.Header.Logid, "in", in)
	bc := s.mg.Get(in.Bcname)
	if bc == nil {
		out := pb.Block{Header: &pb.Header{}}
		out.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE // 拒绝
		return &out, nil
	}
	out := bc.GetBlock(in)

	block := out.GetBlock()
	transactions := block.GetTransactions()
	transactionsFilter := []*pb.Transaction{}
	for _, transaction := range transactions {
		txid := transaction.GetTxid()
		if bc.QueryTxFromForbidden(txid) {
			continue
		}
		transactionsFilter = append(transactionsFilter, transaction)
	}
	if transactions != nil {
		out.Block.Transactions = transactionsFilter
	}
	s.log.Trace("Start to dealwith GetBlock result", "logid", in.Header.Logid,
		"blockid", out.Blockid, "height", out.GetBlock().GetHeight())
	return out, nil
}

// GetBlockChainStatus get systemstatus
func (s *Server) GetBlockChainStatus(ctx context.Context, in *pb.BCStatus) (*pb.BCStatus, error) {
	s.mg.Speed.Add("GetBlockChainStatus")
	if in.Header == nil {
		in.Header = global.GHeader()
	}
	bc := s.mg.Get(in.Bcname)
	if bc == nil {
		out := pb.BCStatus{Header: &pb.Header{}}
		out.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE // 拒绝
		return &out, nil
	}
	return bc.GetBlockChainStatus(in, pb.ViewOption_NONE), nil
}

// ConfirmBlockChainStatus confirm is_trunk
func (s *Server) ConfirmBlockChainStatus(ctx context.Context, in *pb.BCStatus) (*pb.BCTipStatus, error) {
	s.mg.Speed.Add("ConfirmBlockChainStatus")
	if in.Header == nil {
		in.Header = global.GHeader()
	}
	bc := s.mg.Get(in.Bcname)
	if bc == nil {
		out := pb.BCTipStatus{Header: &pb.Header{}}
		out.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE // 拒绝
		return &out, nil
	}
	return bc.ConfirmTipBlockChainStatus(in), nil
}

// GetBlockChains get BlockChains
func (s *Server) GetBlockChains(ctx context.Context, in *pb.CommonIn) (*pb.BlockChains, error) {
	s.mg.Speed.Add("GetBlockChains")
	if in.Header == nil {
		in.Header = global.GHeader()
	}
	out := &pb.BlockChains{Header: &pb.Header{}}
	out.Blockchains = s.mg.GetAll()
	return out, nil
}

// GetSystemStatus get systemstatus
func (s *Server) GetSystemStatus(ctx context.Context, in *pb.CommonIn) (*pb.SystemsStatusReply, error) {
	s.mg.Speed.Add("GetSystemStatus")
	if in.Header == nil {
		in.Header = global.GHeader()
	}
	out := &pb.SystemsStatusReply{Header: &pb.Header{}}
	systemsStatus := &pb.SystemsStatus{
		Header: in.Header,
		Speeds: &pb.Speeds{
			SumSpeeds: make(map[string]float64),
			BcSpeeds:  make(map[string]*pb.BCSpeeds),
		},
	}
	bcs := s.mg.GetAll()
	for _, v := range bcs {
		bc := s.mg.Get(v)
		tmpBcs := &pb.BCStatus{Header: in.Header, Bcname: v}
		bcst := bc.GetBlockChainStatus(tmpBcs, in.ViewOption)
		if _, ok := systemsStatus.Speeds.BcSpeeds[v]; !ok {
			systemsStatus.Speeds.BcSpeeds[v] = &pb.BCSpeeds{}
			systemsStatus.Speeds.BcSpeeds[v].BcSpeed = bc.Speed.GetMaxSpeed()
		}
		systemsStatus.BcsStatus = append(systemsStatus.BcsStatus, bcst)
	}
	systemsStatus.Speeds.SumSpeeds = s.mg.Speed.GetMaxSpeed()
	if in.ViewOption == pb.ViewOption_NONE || in.ViewOption == pb.ViewOption_PEERS {
		systemsStatus.PeerUrls = s.mg.P2pSvr.GetPeerUrls()
	}
	out.SystemsStatus = systemsStatus
	return out, nil
}

// GetNetURL get net url in p2p_base
func (s *Server) GetNetURL(ctx context.Context, in *pb.CommonIn) (*pb.RawUrl, error) {
	if in.Header == nil {
		in.Header = global.GHeader()
	}
	out := &pb.RawUrl{Header: &pb.Header{Logid: in.Header.Logid}}
	out.Header.Error = pb.XChainErrorEnum_SUCCESS
	netURL := s.mg.P2pSvr.GetNetURL()
	out.RawUrl = netURL
	return out, nil
}

// SelectUTXOBySize select utxo inputs depending on size
func (s *Server) SelectUTXOBySize(ctx context.Context, in *pb.UtxoInput) (*pb.UtxoOutput, error) {
	if in.GetHeader() == nil {
		in.Header = global.GHeader()
	}
	out := &pb.UtxoOutput{Header: &pb.Header{Logid: in.Header.Logid}}
	bc := s.mg.Get(in.GetBcname())
	if bc == nil {
		out.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE
		s.log.Warn("failed to merge utxo, bcname not exists", "logid", in.Header.Logid)
		return out, nil
	}
	out.Header.Error = pb.XChainErrorEnum_SUCCESS
	utxos, _, totalSelected, err := bc.Utxovm.SelectUtxosBySize(in.GetAddress(), in.GetPublickey(), in.GetNeedLock(), false)
	if err != nil {
		out.Header.Error = xchaincore.HandlerUtxoError(err)
		s.log.Warn("failed to select utxo", "logid", in.Header.Logid, "error", err.Error())
		return out, nil
	}
	utxoList := []*pb.Utxo{}
	for _, v := range utxos {
		utxo := &pb.Utxo{
			RefTxid:   v.RefTxid,
			RefOffset: v.RefOffset,
			ToAddr:    v.FromAddr,
			Amount:    v.Amount,
		}
		utxoList = append(utxoList, utxo)
		s.log.Trace("Merge utxo list", "refTxid", fmt.Sprintf("%x", v.RefTxid), "refOffset", v.RefOffset, "amount", new(big.Int).SetBytes(v.Amount).String())
	}
	totalSelectedStr := totalSelected.String()
	s.log.Trace("Merge utxo totalSelect", "totalSelect", totalSelectedStr)
	out.UtxoList = utxoList
	out.TotalSelected = totalSelectedStr

	return out, nil
}

// SelectUTXO select utxo inputs depending on amount
func (s *Server) SelectUTXO(ctx context.Context, in *pb.UtxoInput) (*pb.UtxoOutput, error) {
	if in.Header == nil {
		in.Header = global.GHeader()
	}
	out := &pb.UtxoOutput{Header: &pb.Header{Logid: in.Header.Logid}}
	bc := s.mg.Get(in.GetBcname())
	if bc == nil {
		out.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE // 拒绝
		s.log.Warn("failed to select utxo, bcname not exists", "logid", in.Header.Logid)
		return out, nil
	}

	totalNeed, ok := new(big.Int).SetString(in.TotalNeed, 10)
	if !ok {
		out.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE // 拒绝
		return out, nil
	}
	out.Header.Error = pb.XChainErrorEnum_SUCCESS
	utxos, _, totalSelected, err := bc.Utxovm.SelectUtxos(in.GetAddress(), in.GetPublickey(), totalNeed, in.GetNeedLock(), false)
	if err != nil {
		out.Header.Error = xchaincore.HandlerUtxoError(err)
		s.log.Warn("failed to select utxo", "logid", in.Header.Logid, "error", err.Error())
		return out, nil
	}
	utxoList := []*pb.Utxo{}
	for _, v := range utxos {
		utxo := &pb.Utxo{}
		utxo.RefTxid = v.RefTxid
		utxo.Amount = v.Amount
		utxo.RefOffset = v.RefOffset
		utxo.ToAddr = v.FromAddr
		utxoList = append(utxoList, utxo)
		s.log.Trace("Select utxo list", "refTxid", fmt.Sprintf("%x", v.RefTxid), "refOffset", v.RefOffset, "amount", new(big.Int).SetBytes(v.Amount).String())
	}
	totalSelectedStr := totalSelected.String()
	s.log.Trace("Select utxo totalSelect", "totalSelect", totalSelectedStr)
	out.UtxoList = utxoList
	out.TotalSelected = totalSelectedStr
	return out, nil
}

// DeployNativeCode deploy native contract
func (s *Server) DeployNativeCode(ctx context.Context, request *pb.DeployNativeCodeRequest) (*pb.DeployNativeCodeResponse, error) {
	return nil, errors.New("old deploy method disabled, using xkernel.Deploy method instead")
}

// NativeCodeStatus get native contract status
func (s *Server) NativeCodeStatus(ctx context.Context, request *pb.NativeCodeStatusRequest) (*pb.NativeCodeStatusResponse, error) {
	return nil, errors.New("old status method disabled")
}

// DposCandidates get dpos candidates
func (s *Server) DposCandidates(ctx context.Context, request *pb.DposCandidatesRequest) (*pb.DposCandidatesResponse, error) {
	bc := s.mg.Get(request.GetBcname())
	if request.Header == nil {
		request.Header = global.GHeader()
	}

	response := &pb.DposCandidatesResponse{Header: &pb.Header{Logid: request.Header.Logid}}
	if bc == nil {
		response.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE // 拒绝
		s.log.Warn("DposCandidates failed to get blockchain", "logid", request.Header.Logid)
		return response, nil
	}
	if bc.GetConsType() != consensus.ConsensusTypeTdpos {
		response.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE // 拒绝
		s.log.Warn("DposCandidates failed to check consensus type", "logid", request.Header.Logid)
		return response, errors.New("The consensus is not tdpos")
	}

	candidates, err := bc.GetDposCandidates()
	if err != nil {
		response.Header.Error = pb.XChainErrorEnum_DPOS_QUERY_ERROR
		s.log.Warn("DposCandidates error", "logid", request.Header.Logid, "error", err)
		return response, err
	}
	response.Header.Error = pb.XChainErrorEnum_SUCCESS
	response.CandidatesInfo = candidates
	return response, nil
}

// DposNominateRecords get dpos 提名者提名记录
func (s *Server) DposNominateRecords(ctx context.Context, request *pb.DposNominateRecordsRequest) (*pb.DposNominateRecordsResponse, error) {
	bc := s.mg.Get(request.GetBcname())
	if request.Header == nil {
		request.Header = global.GHeader()
	}
	response := &pb.DposNominateRecordsResponse{Header: &pb.Header{Logid: request.Header.Logid}}

	if bc == nil {
		response.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE // 拒绝
		s.log.Warn("DposNominateRecords failed to get blockchain", "logid", request.Header.Logid)
		return response, nil
	}

	if bc.GetConsType() != consensus.ConsensusTypeTdpos {
		response.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE // 拒绝
		s.log.Warn("DposNominateRecords failed to check consensus type", "logid", request.Header.Logid)
		return response, errors.New("The consensus is not tdpos")
	}
	s.log.Info("DposNominateRecords GetDposNominateRecords")
	nominateRecords, err := bc.GetDposNominateRecords(request.Address)
	if err != nil {
		response.Header.Error = pb.XChainErrorEnum_DPOS_QUERY_ERROR
		s.log.Warn("DposNominateRecords error", "logid", request.Header.Logid, "error", err)
		return response, err
	}
	response.Header.Error = pb.XChainErrorEnum_SUCCESS
	response.NominateRecords = nominateRecords
	return response, nil
}

// DposNomineeRecords 候选人被提名记录
func (s *Server) DposNomineeRecords(ctx context.Context, request *pb.DposNomineeRecordsRequest) (*pb.DposNomineeRecordsResponse, error) {
	bc := s.mg.Get(request.GetBcname())
	if request.Header == nil {
		request.Header = global.GHeader()
	}
	response := &pb.DposNomineeRecordsResponse{Header: &pb.Header{Logid: request.Header.Logid}}
	if bc == nil {
		response.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE // 拒绝
		s.log.Warn("DposNominatedRecords failed to get blockchain", "logid", request.Header.Logid)
		return response, nil
	}

	if bc.GetConsType() != consensus.ConsensusTypeTdpos {
		response.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE // 拒绝
		s.log.Warn("DposNominatedRecords failed to check consensus type", "logid", request.Header.Logid)
		return response, errors.New("The consensus is not tdpos")
	}

	txid, err := bc.GetDposNominatedRecords(request.Address)
	if err != nil {
		response.Header.Error = pb.XChainErrorEnum_DPOS_QUERY_ERROR
		s.log.Warn("DposNominatedRecords error", "logid", request.Header.Logid, "error", err)
		return response, err
	}
	response.Header.Error = pb.XChainErrorEnum_SUCCESS
	response.Txid = txid
	return response, nil
}

// DposVoteRecords 选民投票记录
func (s *Server) DposVoteRecords(ctx context.Context, request *pb.DposVoteRecordsRequest) (*pb.DposVoteRecordsResponse, error) {
	bc := s.mg.Get(request.GetBcname())
	if request.Header == nil {
		request.Header = global.GHeader()
	}
	response := &pb.DposVoteRecordsResponse{Header: &pb.Header{Logid: request.Header.Logid}}
	if bc == nil {
		response.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE // 拒绝
		s.log.Warn("DposVoteRecords failed to get blockchain", "logid", request.Header.Logid)
		return response, nil
	}

	if bc.GetConsType() != consensus.ConsensusTypeTdpos {
		response.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE // 拒绝
		s.log.Warn("DposVoteRecords failed to check consensus type", "logid", request.Header.Logid)
		return response, errors.New("The consensus is not tdpos")
	}

	voteRecords, err := bc.GetDposVoteRecords(request.Address)
	if err != nil {
		response.Header.Error = pb.XChainErrorEnum_DPOS_QUERY_ERROR
		s.log.Warn("DposVoteRecords error", "logid", request.Header.Logid, "error", err)
		return response, err
	}
	response.Header.Error = pb.XChainErrorEnum_SUCCESS
	response.VoteTxidRecords = voteRecords
	return response, nil
}

// DposVotedRecords 候选人被投票记录
func (s *Server) DposVotedRecords(ctx context.Context, request *pb.DposVotedRecordsRequest) (*pb.DposVotedRecordsResponse, error) {
	bc := s.mg.Get(request.GetBcname())
	if request.Header == nil {
		request.Header = global.GHeader()
	}
	response := &pb.DposVotedRecordsResponse{Header: &pb.Header{Logid: request.Header.Logid}}
	if bc == nil {
		response.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE // 拒绝
		s.log.Warn("DposVotedRecords failed to get blockchain", "logid", request.Header.Logid)
		return response, nil
	}

	if bc.GetConsType() != consensus.ConsensusTypeTdpos {
		response.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE // 拒绝
		s.log.Warn("DposVoteRecords failed to check consensus type", "logid", request.Header.Logid)
		return response, errors.New("The consensus is not tdpos")
	}
	votedRecords, err := bc.GetDposVotedRecords(request.Address)
	if err != nil {
		response.Header.Error = pb.XChainErrorEnum_DPOS_QUERY_ERROR
		s.log.Warn("GetDposVotedRecords error", "logid", request.Header.Logid, "error", err)
		return response, err
	}
	response.Header.Error = pb.XChainErrorEnum_SUCCESS
	response.VotedTxidRecords = votedRecords
	return response, nil
}

// DposCheckResults get dpos 检查结果
func (s *Server) DposCheckResults(ctx context.Context, request *pb.DposCheckResultsRequest) (*pb.DposCheckResultsResponse, error) {
	bc := s.mg.Get(request.GetBcname())
	if request.Header == nil {
		request.Header = global.GHeader()
	}

	response := &pb.DposCheckResultsResponse{Header: &pb.Header{Logid: request.Header.Logid}}
	if bc == nil {
		response.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE // 拒绝
		s.log.Warn("DposVotedRecords failed to get blockchain", "logid", request.Header.Logid)
		return response, nil
	}

	if bc.GetConsType() != consensus.ConsensusTypeTdpos {
		response.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE // 拒绝
		s.log.Warn("DposVoteRecords failed to check consensus type", "logid", request.Header.Logid)
		return response, errors.New("The consensus is not tdpos")
	}

	checkResult, err := bc.GetCheckResults(request.Term)
	if err != nil {
		response.Header.Error = pb.XChainErrorEnum_DPOS_QUERY_ERROR
		s.log.Warn("DposCheckResults error", "logid", request.Header.Logid, "error", err)
		return response, err
	}
	response.Term = request.Term
	response.CheckResult = checkResult
	return response, nil
}

// DposStatus get dpos current status
func (s *Server) DposStatus(ctx context.Context, request *pb.DposStatusRequest) (*pb.DposStatusResponse, error) {
	bc := s.mg.Get(request.GetBcname())
	if request.Header == nil {
		request.Header = global.GHeader()
	}
	response := &pb.DposStatusResponse{Header: &pb.Header{Logid: request.Header.Logid}, Status: &pb.DposStatus{}}
	if bc == nil {
		response.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE
		s.log.Warn("DposStatus failed  to get blockchain", "logid", request.Header.Logid)
		return response, nil
	}

	if bc.GetConsType() != consensus.ConsensusTypeTdpos {
		response.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE
		s.log.Warn("DposStatus failed to check consensus type", "logid", request.Header.Logid)
		return response, errors.New("The consensus is not tdpos")
	}

	status := bc.GetConsStatus()
	response.Status.Term = status.Term
	response.Status.BlockNum = status.BlockNum
	response.Status.Proposer = status.Proposer
	checkResult, err := bc.GetCheckResults(status.Term)
	if err != nil {
		response.Header.Error = pb.XChainErrorEnum_DPOS_QUERY_ERROR
		s.log.Warn("DposStatus error", "logid", request.Header.Logid, "error", err)
		return response, err
	}
	response.Status.CheckResult = checkResult
	response.Status.ProposerNum = int64(len(checkResult))
	return response, nil
}

// PreExecWithSelectUTXO preExec + selectUtxo
func (s *Server) PreExecWithSelectUTXO(ctx context.Context, request *pb.PreExecWithSelectUTXORequest) (*pb.PreExecWithSelectUTXOResponse, error) {
	// verify input param
	if request == nil {
		return nil, errors.New("request is invalid")
	}
	if request.Header == nil {
		request.Header = global.GHeader()
	}

	// initialize output
	responses := &pb.PreExecWithSelectUTXOResponse{Header: &pb.Header{Logid: request.Header.Logid}}
	responses.Bcname = request.GetBcname()
	// for PreExec
	preExecRequest := request.GetRequest()
	fee := int64(0)
	if preExecRequest != nil {
		preExecRequest.Header = request.Header
		invokeRPCResponse, preErr := s.PreExec(ctx, preExecRequest)
		if preErr != nil {
			return nil, preErr
		}
		invokeResponse := invokeRPCResponse.GetResponse()
		responses.Response = invokeResponse
		fee = responses.Response.GetGasUsed()
	}

	totalAmount := request.GetTotalAmount() + fee

	if totalAmount > 0 {
		utxoInput := &pb.UtxoInput{
			Bcname:    request.GetBcname(),
			Address:   request.GetAddress(),
			TotalNeed: strconv.FormatInt(totalAmount, 10),
			Publickey: request.GetSignInfo().GetPublicKey(),
			UserSign:  request.GetSignInfo().GetSign(),
			NeedLock:  request.GetNeedLock(),
		}
		if ok := validUtxoAccess(utxoInput, s.mg.Get(utxoInput.GetBcname()), request.GetTotalAmount()); !ok {
			return nil, errors.New("validUtxoAccess failed")
		}
		utxoOutput, selectErr := s.SelectUTXO(ctx, utxoInput)
		if selectErr != nil {
			return nil, selectErr
		}
		if utxoOutput.Header.Error != pb.XChainErrorEnum_SUCCESS {
			return nil, common.ServerError{utxoOutput.Header.Error}
		}
		responses.UtxoOutput = utxoOutput
	}

	return responses, nil
}

// PreExec smart contract preExec process
func (s *Server) PreExec(ctx context.Context, request *pb.InvokeRPCRequest) (*pb.InvokeRPCResponse, error) {
	s.log.Trace("Got PreExec req", "req", request)
	bc := s.mg.Get(request.GetBcname())
	if request.Header == nil {
		request.Header = global.GHeader()
	}
	rsps := &pb.InvokeRPCResponse{Header: &pb.Header{Logid: request.Header.Logid}}
	if bc == nil {
		rsps.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE // 拒绝
		s.log.Warn("failed to get blockchain before query", "logid", request.Header.Logid)
		return rsps, nil
	}
	hd := &global.XContext{Timer: global.NewXTimer()}
	vmResponse, err := bc.PreExec(request, hd)
	if err != nil {
		return nil, err
	}
	txInputs := vmResponse.GetInputs()
	for _, txInput := range txInputs {
		if bc.QueryTxFromForbidden(txInput.GetRefTxid()) {
			return rsps, errors.New("RefTxid has been forbidden")
		}
	}
	rsps.Response = vmResponse
	s.log.Info("PreExec", "logid", request.Header.Logid, "cost", hd.Timer.Print())
	return rsps, nil
}

// GetBlockByHeight  get trunk block by height
func (s *Server) GetBlockByHeight(ctx context.Context, in *pb.BlockHeight) (*pb.Block, error) {
	if in.Header == nil {
		in.Header = global.GHeader()
	}
	s.log.Trace("Start to get dealwith GetBlockByHeight", "logid", in.Header.Logid, "bcname", in.Bcname, "height", in.Height)
	bc := s.mg.Get(in.Bcname)
	if bc == nil {
		out := pb.Block{Header: &pb.Header{}}
		out.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE // 拒绝
		return &out, nil
	}
	out := bc.GetBlockByHeight(in)

	block := out.GetBlock()
	transactions := block.GetTransactions()
	transactionsFilter := []*pb.Transaction{}
	for _, transaction := range transactions {
		txid := transaction.GetTxid()
		if bc.QueryTxFromForbidden(txid) {
			continue
		}
		transactionsFilter = append(transactionsFilter, transaction)
	}
	if transactions != nil {
		out.Block.Transactions = transactionsFilter
	}

	s.log.Trace("GetBlockByHeight result", "logid", in.Header.Logid, "bcname", in.Bcname, "height", in.Height,
		"blockid", out.GetBlockid())
	return out, nil
}

func (s *Server) GetAccountByAK(ctx context.Context, request *pb.AK2AccountRequest) (*pb.AK2AccountResponse, error) {
	if request.Header == nil {
		request.Header = global.GHeader()
	}
	bc := s.mg.Get(request.Bcname)
	if bc == nil {
		out := pb.AK2AccountResponse{Header: &pb.Header{}}
		out.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE // 拒绝
		return &out, nil
	}
	out := &pb.AK2AccountResponse{
		Bcname: request.Bcname,
		Header: global.GHeader(),
	}
	accounts, err := bc.QueryAccountContainAK(request.GetAddress())
	if err != nil || accounts == nil {
		return out, err
	}
	out.Account = accounts
	return out, err
}

// GetAddressContracts get contracts of accounts contain a specific address
func (s *Server) GetAddressContracts(ctx context.Context, request *pb.AddressContractsRequest) (*pb.AddressContractsResponse, error) {
	if request.Header == nil {
		request.Header = global.GHeader()
	}
	res := &pb.AddressContractsResponse{
		Header: &pb.Header{
			Error: pb.XChainErrorEnum_SUCCESS,
			Logid: request.GetHeader().GetLogid(),
		},
	}
	bc := s.mg.Get(request.GetBcname())
	if bc == nil {
		res.Header.Error = pb.XChainErrorEnum_CONNECT_REFUSE
		s.log.Warn("GetAddressContracts:failed to get blockchain before query", "logid", res.Header.Logid)
		return res, nil
	}

	// get all accounts which contains this address
	accounts, err := bc.QueryAccountContainAK(request.GetAddress())
	if err != nil || accounts == nil {
		res.Header.Error = pb.XChainErrorEnum_SERVICE_REFUSED_ERROR
		s.log.Warn("GetAddressContracts: error occurred", "logid", res.Header.Logid, "error", err)
		return res, err
	}

	// get contracts for each account
	res.Contracts = make(map[string]*pb.ContractList)
	for _, account := range accounts {
		contracts, err := bc.GetAccountContractsStatus(account, request.GetNeedContent())
		if err != nil {
			s.log.Warn("GetAddressContracts partial account error", "logid", res.Header.Logid, "error", err)
			continue
		}
		if len(contracts) > 0 {
			res.Contracts[account] = &pb.ContractList{
				ContractStatus: contracts,
			}
		}
	}

	return res, nil
}

// Output access log and cost time
type rpcAccessLog struct {
	xlogf  *xlog.LogFitter
	xtimer *global.XTimer
}

type HeaderInterface interface {
	GetHeader() *pb.Header
}

func (s *Server) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (resp interface{}, err error) {

		// panic recover
		defer func() {
			if e := recover(); e != nil {
				s.log.Error("Happen panic.", "error", e, "rpc_method", info.FullMethod)
			}
		}()

		// padding header
		if req.(HeaderInterface).GetHeader() == nil {
			header := reflect.ValueOf(req).Elem().FieldByName("Header")
			if header.IsValid() && header.IsNil() && header.CanSet() {
				header.Set(reflect.ValueOf(global.GHeader()))
			}
		}

		// Output access log and init timer
		alog := s.accessLog(s.log, req.(HeaderInterface).GetHeader().GetLogid(),
			"rpc_method", info.FullMethod)

		// handle request
		resp, err = handler(ctx, req)

		// output ending log
		s.endingLog(alog, "rpc_method", info.FullMethod,
			"resp_error", resp.(HeaderInterface).GetHeader().GetError())
		return resp, err
	}
}

// output rpc request access log
func (s *Server) accessLog(lg xlog.LogInterface, logId string, others ...interface{}) *rpcAccessLog {
	// check param
	if lg == nil {
		return nil
	}

	xlf, _ := xlog.NewLogger(lg, logId)
	alog := &rpcAccessLog{
		xlogf:  xlf,
		xtimer: global.NewXTimer(),
	}

	logFields := make([]interface{}, 0)
	logFields = append(logFields, others...)

	alog.xlogf.Info("xchain rpc access request", logFields...)
	return alog
}

// output rpc request ending log
func (t *Server) endingLog(alog *rpcAccessLog, others ...interface{}) {
	if alog == nil || alog.xlogf == nil || alog.xtimer == nil {
		return
	}

	logFields := make([]interface{}, 0)
	logFields = append(logFields, "cost_time", alog.xtimer.Print())
	logFields = append(logFields, others...)
	alog.xlogf.Notice("xchain rpc service done", logFields...)
}

func startTCPServer(xchainmg *xchaincore.XChainMG) error {
	var (
		cfg                     = xchainmg.Cfg
		log                     = xchainmg.Log
		isTLS                   = cfg.TCPServer.TLS
		svr                     = Server{log: log, mg: xchainmg, dedupCache: common.NewLRUCache(cfg.DedupCacheSize), dedupTimeLimit: cfg.DedupTimeLimit}
		unaryServerInterceptors = make([]grpc.UnaryServerInterceptor, 0)
		rpcOptions              []grpc.ServerOption
	)

	unaryServerInterceptors = append(unaryServerInterceptors, svr.UnaryServerInterceptor())
	if cfg.TCPServer.MetricPort != "" {
		// add prometheus support
		rpcOptions = append(rpcOptions,
			grpc.StreamInterceptor(grpc_prometheus.StreamServerInterceptor),
		)
		unaryServerInterceptors = append(unaryServerInterceptors, grpc_prometheus.UnaryServerInterceptor)
	}

	rpcOptions = append(rpcOptions,
		middleware.WithUnaryServerChain(unaryServerInterceptors...),
		grpc.MaxMsgSize(cfg.TCPServer.MaxMsgSize),
		grpc.ReadBufferSize(cfg.TCPServer.ReadBufferSize),
		grpc.InitialWindowSize(cfg.TCPServer.InitialWindowSize),
		grpc.InitialConnWindowSize(cfg.TCPServer.InitialConnWindowSize),
		grpc.WriteBufferSize(cfg.TCPServer.WriteBufferSize),
	)

	if isTLS {
		log.Trace("start tls rpc server")
		tlsPath := cfg.TCPServer.TLSPath
		bs, err := ioutil.ReadFile(tlsPath + "/cert.crt")
		if err != nil {
			return err
		}
		certPool := x509.NewCertPool()
		ok := certPool.AppendCertsFromPEM(bs)
		if !ok {
			return err
		}

		certificate, err := tls.LoadX509KeyPair(tlsPath+"/key.pem", tlsPath+"/private.key")

		creds := credentials.NewTLS(
			&tls.Config{
				ServerName:   cfg.TCPServer.MServerName,
				Certificates: []tls.Certificate{certificate},
				RootCAs:      certPool,
				ClientCAs:    certPool,
				ClientAuth:   tls.RequireAndVerifyClientCert,
			})

		l, err := net.Listen("tcp", cfg.TCPServer.HTTPSPort)
		if err != nil {
			return err
		}
		// Copy rpc options
		options := append([]grpc.ServerOption{}, rpcOptions...)
		options = append(options, grpc.Creds(creds))
		s := grpc.NewServer(options...)
		pb.RegisterXchainServer(s, &svr)
		reflection.Register(s)
		log.Trace("start tls rpc server")
		go func() {
			err := s.Serve(l)
			if err != nil {
				panic(err)
			}
		}()
	}

	log.Trace("start rpc server")
	s := grpc.NewServer(rpcOptions...)
	pb.RegisterXchainServer(s, &svr)
	if cfg.XEndorser.Enable {
		endorser, err := xendorser.GetXEndorser(cfg.XEndorser.Module)
		if err != nil {
			panic(err)
		}
		params := map[string]interface{}{}
		params["server"] = &svr
		endorser.Init(cfg.XEndorser.ConfPath, params)
		pb.RegisterXendorserServer(s, endorser)
	}
	if cfg.TCPServer.MetricPort != "" {
		grpc_prometheus.EnableHandlingTimeHistogram(
			grpc_prometheus.WithHistogramBuckets([]float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}),
		)
		// 考虑到tls rpc server跟普通server共用一个XchainService，所以只需要注册一次prometheus就行
		// 因为两者的ServerOption都包含了prometheus的拦截器，所以都可以监控到
		// Must be called after RegisterXchainServer
		grpc_prometheus.Register(s)
		http.Handle("/metrics", promhttp.Handler())
		go func() {
			panic(http.ListenAndServe(cfg.TCPServer.MetricPort, nil))
		}()
	}

	// event involved rpc
	pubsubService := &PubsubService{EventService: xchainmg.EventService}
	go pubsubService.EventService.Start()
	pb.RegisterPubsubServiceServer(s, pubsubService)

	lis, err := net.Listen("tcp", cfg.TCPServer.Port)
	if err != nil {
		log.Error("failed to listen: ", "error", err.Error())
		return err
	}
	reflection.Register(s)
	if err := s.Serve(lis); err != nil {
		log.Error("failed to serve: ", "error", err.Error())
		return err
	}
	return nil
}

// SerRun xchain server start entrance
func SerRun(xchainmg *xchaincore.XChainMG) {
	err := startTCPServer(xchainmg)
	if err != nil {
		close(xchainmg.Quit)
	}
}
