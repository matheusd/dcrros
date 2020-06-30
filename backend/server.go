package backend

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/coinbase/rosetta-sdk-go/asserter"
	rserver "github.com/coinbase/rosetta-sdk-go/server"
	rtypes "github.com/coinbase/rosetta-sdk-go/types"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/lru"
	"github.com/decred/dcrd/rpcclient/v6"
	"github.com/decred/dcrros/backend/backenddb"
	"github.com/decred/dcrros/backend/internal/badgerdb"
	"github.com/decred/dcrros/backend/internal/memdb"
)

const (
	// rosettaVersion is the version of the rosetta spec this backend
	// currently implements.
	rosettaVersion = "1.3.1"
)

type DBType string

const (
	DBTypeMem       DBType = "mem"
	DBTypeBadger    DBType = "badger"
	DBTypeBadgerMem DBType = "badgermem"
)

func SupportedDBTypes() []DBType {
	return []DBType{DBTypeMem, DBTypeBadger, DBTypeBadgerMem}
}

type ServerConfig struct {
	ChainParams *chaincfg.Params
	DcrdCfg     *rpcclient.ConnConfig
	DBType      DBType
	DBDir       string
}

type Server struct {
	c           *rpcclient.Client
	ctx         context.Context
	chainParams *chaincfg.Params
	asserter    *asserter.Asserter
	network     *rtypes.NetworkIdentifier
	db          backenddb.DB

	blocks      *lru.KVCache
	blockHashes *lru.KVCache
	accountTxs  *lru.KVCache
	rawTxs      *lru.KVCache

	// The given mtx mutex protects the following fields.
	mtx         sync.Mutex
	active      bool
	dcrdVersion string
}

func NewServer(ctx context.Context, cfg *ServerConfig) (*Server, error) {
	network := &rtypes.NetworkIdentifier{
		Blockchain: "decred",
		Network:    cfg.ChainParams.Name,
	}

	astr, err := asserter.NewServer([]*rtypes.NetworkIdentifier{network})
	if err != nil {
		return nil, err
	}

	blockCache := lru.NewKVCache(0)
	blockHashCache := lru.NewKVCache(0)
	accountTxsCache := lru.NewKVCache(0)
	txsCache := lru.NewKVCache(0)

	var db backenddb.DB
	switch cfg.DBType {
	case DBTypeMem:
		db, err = memdb.NewMemDB()
	case DBTypeBadger:
		db, err = badgerdb.NewBadgerDB(cfg.DBDir)
	case DBTypeBadgerMem:
		db, err = badgerdb.NewBadgerDB("")
	default:
		err = errors.New("unknown db type")
	}
	if err != nil {
		return nil, err
	}

	s := &Server{
		chainParams: cfg.ChainParams,
		asserter:    astr,
		network:     network,
		ctx:         ctx,
		blocks:      &blockCache,
		blockHashes: &blockHashCache,
		accountTxs:  &accountTxsCache,
		rawTxs:      &txsCache,
		db:          db,
	}

	// We make a copy of the passed config because we change some of the
	// parameters locally to ensure they are configured as needed by the
	// Server struct.
	connCfg := *cfg.DcrdCfg
	connCfg.DisableConnectOnNew = true
	connCfg.DisableAutoReconnect = false
	connCfg.HTTPPostMode = false
	s.c, err = rpcclient.New(&connCfg, s.ntfnHandlers())
	if err != nil {
		db.Close()
		return nil, err
	}

	return s, nil
}

func (s *Server) Active() bool {
	s.mtx.Lock()
	active := s.active
	s.mtx.Unlock()
	return active
}

func (s *Server) onDcrdConnected() {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	// Ideally these would be done on a onDcrdDisconnected() callback but
	// rpcclient doesn't currently offer that.
	s.active = false
	s.dcrdVersion = ""

	svrLog.Debugf("Reconnected to the dcrd instance")
	version, err := checkDcrd(s.ctx, s.c, s.chainParams)
	if err != nil {
		svrLog.Error(err)
		svrLog.Infof("Disabling server operations")
		return
	}

	s.active = true
	s.dcrdVersion = version
}

func (s *Server) ntfnHandlers() *rpcclient.NotificationHandlers {
	return &rpcclient.NotificationHandlers{
		OnClientConnected: s.onDcrdConnected,
	}
}

func (s *Server) Routers() []rserver.Router {
	return []rserver.Router{
		rserver.NewNetworkAPIController(s, s.asserter),
		rserver.NewBlockAPIController(s, s.asserter),
		rserver.NewMempoolAPIController(s, s.asserter),
		rserver.NewConstructionAPIController(s, s.asserter),
		rserver.NewAccountAPIController(s, s.asserter),
	}
}

// Run starts all service goroutines and blocks until the passed context is
// canceled.
//
// NOTE: the passed context MUST be the same one passed for New() otherwise the
// server's behavior is undefined.
func (s *Server) Run(ctx context.Context) error {
	go s.c.Connect(ctx, true)
	time.Sleep(time.Millisecond * 100)

	err := s.preProcessAccounts(ctx)
	if err != nil {
		s.db.Close()
		return err
	}

	select {
	case <-ctx.Done():
	}
	s.db.Close()
	return ctx.Err()
}
