package service

import (
	"sync"
	"time"

	"github.com/avast/retry-go/v4"
	bbncli "github.com/babylonchain/btc-validator/bbnclient"
	cfg "github.com/babylonchain/btc-validator/valcfg"
	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/sirupsen/logrus"
)

var (
	// TODO: Maybe configurable?
	RtyAttNum = uint(5)
	RtyAtt    = retry.Attempts(RtyAttNum)
	RtyDel    = retry.Delay(time.Millisecond * 400)
	RtyErr    = retry.LastErrorOnly(true)
)

const (
	pollInterval = 5 * time.Second

	// TODO: Maybe configurable?
	maxFailedCycles = 20
)

type ChainPoller struct {
	startOnce sync.Once
	stopOnce  sync.Once
	wg        sync.WaitGroup
	quit      chan struct{}

	bc            bbncli.BabylonClient
	cfg           *cfg.PollerConfig
	blockInfoChan chan *BlockInfo
	logger        *logrus.Logger
}

type PollerState struct {
	HeaderToRetrieve uint64
	FailedCycles     uint64
}

type BlockInfo struct {
	Height         uint64
	LastCommitHash []byte
}

func NewChainPoller(
	logger *logrus.Logger,
	cfg *cfg.PollerConfig,
	bc bbncli.BabylonClient,
) *ChainPoller {
	return &ChainPoller{
		logger:        logger,
		cfg:           cfg,
		bc:            bc,
		blockInfoChan: make(chan *BlockInfo, cfg.BufferSize),
		quit:          make(chan struct{}),
	}
}

func (cp *ChainPoller) Start() error {
	var startErr error
	cp.startOnce.Do(func() {
		initialBlockToGet, err := cp.initPoller()

		if err != nil {
			startErr = err
			return
		}

		cp.wg.Add(1)
		go cp.pollChain(PollerState{
			HeaderToRetrieve: initialBlockToGet,
			FailedCycles:     0,
		})
	})
	return startErr
}

func (cp *ChainPoller) Stop() error {
	cp.stopOnce.Do(func() {
		close(cp.quit)
		cp.wg.Wait()
	})

	return nil
}

// Return read only channel for incoming blocks
// TODO: Handle the case when there is more than one consumer. Currently with more than
// one consumer blocks most probably will be received out of order to those consumers.
func (cp *ChainPoller) GetBlockInfoChan() <-chan *BlockInfo {
	return cp.blockInfoChan
}

func (cp *ChainPoller) nodeStatusWithRetry() (*ctypes.ResultStatus, error) {
	var response *ctypes.ResultStatus
	if err := retry.Do(func() error {
		status, err := cp.bc.QueryNodeStatus()
		if err != nil {
			return err
		}
		response = status
		return nil
	}, RtyAtt, RtyDel, RtyErr, retry.OnRetry(func(n uint, err error) {
		cp.logger.WithFields(logrus.Fields{
			"attempt":      n + 1,
			"max_attempts": RtyAttNum,
			"error":        err,
		}).Debug("Failed to query babylon For the latest height")
	})); err != nil {
		return nil, err
	}
	return response, nil
}

func (cp *ChainPoller) headerWithRetry(height uint64) (*ctypes.ResultHeader, error) {
	var response *ctypes.ResultHeader
	if err := retry.Do(func() error {
		headerResult, err := cp.bc.QueryHeader(int64(height))
		if err != nil {
			return err
		}
		response = headerResult
		return nil
	}, RtyAtt, RtyDel, RtyErr, retry.OnRetry(func(n uint, err error) {
		cp.logger.WithFields(logrus.Fields{
			"attempt":      n + 1,
			"max_attempts": RtyAttNum,
			"error":        err,
		}).Debug("Failed to query babylon For the latest header")
	})); err != nil {
		return nil, err
	}
	return response, nil
}

func (cp *ChainPoller) initPoller() (uint64, error) {
	// Infinite retry to get initial latest height
	// TODO: Add possible cancelation or timeout for starting node
	var currentBestChainHeight uint64
	for {
		status, err := cp.nodeStatusWithRetry()
		if err != nil {
			cp.logger.WithFields(logrus.Fields{
				"error": err,
			}).Error("Failed to query babylon For the latest status")
			continue
		}

		currentBestChainHeight = uint64(status.SyncInfo.LatestBlockHeight)
		break
	}

	var initialBlockToGet uint64

	if currentBestChainHeight == 0 {
		initialBlockToGet = 1
	} else if cp.cfg.StartingHeight > currentBestChainHeight {
		initialBlockToGet = currentBestChainHeight
	} else {
		initialBlockToGet = cp.cfg.StartingHeight
	}

	return initialBlockToGet, nil
}

func (cp *ChainPoller) pollChain(initialState PollerState) {
	defer cp.wg.Done()

	var state = initialState

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		// TODO: Handlig of request cancellation, as otherwise shutdown will be blocked
		// until request is finished
		result, err := cp.headerWithRetry(state.HeaderToRetrieve)

		if err == nil && result.Header != nil {
			// no error and we got the header we wanted to get, bump the state and push
			// notification about data
			state.HeaderToRetrieve = state.HeaderToRetrieve + 1
			state.FailedCycles = 0

			cp.logger.WithFields(logrus.Fields{
				"height": result.Header.Height,
			}).Info("Retrieved header from babylon")

			// Push the data to the channel.
			// If the cosumers are to slow i.e the buffer is full, this will block and we will
			// stop retrieving data from the node.
			cp.blockInfoChan <- &BlockInfo{
				Height:         uint64(result.Header.Height),
				LastCommitHash: result.Header.LastCommitHash,
			}

		} else if err == nil && result.Header == nil {
			// no error but header is nil, means the header is not found on node
			state.FailedCycles = state.FailedCycles + 1

			cp.logger.WithFields(logrus.Fields{
				"error":        err,
				"currFailures": state.FailedCycles,
				"headerToGet":  state.HeaderToRetrieve,
			}).Error("Failed to retrieve header from babylon.Header did not produced yet")

		} else {
			state.FailedCycles = state.FailedCycles + 1

			cp.logger.WithFields(logrus.Fields{
				"error":        err,
				"currFailures": state.FailedCycles,
				"headerToGet":  state.HeaderToRetrieve,
			}).Error("Failed to query babylon For the latest header")
		}

		if state.FailedCycles > maxFailedCycles {
			cp.logger.Fatal("Reached max failed cycles, exiting")
		}

		select {
		case <-cp.quit:
			return
		case <-ticker.C:
			ticker.Reset(pollInterval)
		}
	}
}