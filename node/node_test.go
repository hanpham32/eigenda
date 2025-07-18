package node_test

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/docker/go-units"
	"github.com/gammazero/workerpool"

	clientsmock "github.com/Layr-Labs/eigenda/api/clients/v2/mock"
	"github.com/Layr-Labs/eigenda/common"
	"github.com/Layr-Labs/eigenda/common/geth"
	"github.com/Layr-Labs/eigenda/core"
	coremock "github.com/Layr-Labs/eigenda/core/mock"
	v2 "github.com/Layr-Labs/eigenda/core/v2"
	"github.com/Layr-Labs/eigenda/node"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

var (
	privateKey = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	op0        = [32]byte{0}
	op3        = [32]byte{3}

	blobParams = &core.BlobVersionParameters{
		NumChunks:       8192,
		CodingRate:      8,
		MaxNumOperators: 2048,
	}
	blobParamsMap = map[v2.BlobVersion]*core.BlobVersionParameters{
		0: blobParams,
	}
)

type components struct {
	node        *node.Node
	tx          *coremock.MockWriter
	relayClient *clientsmock.MockRelayClient
}

func newComponents(t *testing.T, operatorID [32]byte) *components {
	dbPath := t.TempDir()
	keyPair, err := core.GenRandomBlsKeys()
	if err != nil {
		panic("failed to create a BLS Key")
	}
	config := &node.Config{
		Timeout:                   10 * time.Second,
		ExpirationPollIntervalSec: 1,
		QuorumIDList:              []core.QuorumID{0},
		DbPath:                    dbPath,
		ID:                        operatorID,
		NumBatchValidators:        runtime.GOMAXPROCS(0),
		EnableNodeApi:             false,
		EnableMetrics:             false,
		RegisterNodeAtStart:       false,
		RelayMaxMessageSize:       units.GiB,
	}
	loggerConfig := common.DefaultLoggerConfig()
	logger, err := common.NewLogger(loggerConfig)
	if err != nil {
		panic("failed to create a logger")
	}

	err = os.MkdirAll(config.DbPath, os.ModePerm)
	if err != nil {
		panic("failed to create a directory for levelDB")
	}
	tx := &coremock.MockWriter{}

	mockVal := coremock.NewMockShardValidator()
	mockVal.On("ValidateBatch", mock.Anything, mock.Anything, mock.Anything).Return(nil)

	chainState, _ := coremock.MakeChainDataMock(map[uint8]int{
		0: 4,
		1: 4,
		2: 3,
	})

	store, err := node.NewLevelDBStore(
		dbPath,
		logger,
		nil,
		1e9,
		true,
		false,
		1e9)
	if err != nil {
		panic("failed to create a new levelDB store")
	}
	t.Cleanup(func() {
		if err := os.Remove(dbPath); err != nil {
			t.Log("failed to remove dbPath:", dbPath, "error:", err)
		}
	})
	n := &node.Node{
		Config:         config,
		Logger:         logger,
		KeyPair:        keyPair,
		Metrics:        nil,
		Store:          store,
		ChainState:     chainState,
		Validator:      mockVal,
		Transactor:     tx,
		DownloadPool:   workerpool.New(1),
		ValidationPool: workerpool.New(1),
	}
	n.BlobVersionParams.Store(v2.NewBlobVersionParameterMap(blobParamsMap))
	return &components{
		node:        n,
		tx:          tx,
		relayClient: clientsmock.NewRelayClient(),
	}
}

func TestNodeStartNoAddress(t *testing.T) {
	c := newComponents(t, op0)
	c.node.Config.RegisterNodeAtStart = false

	c.tx.On("GetOperatorSocket", mock.Anything).Return("", errors.New("failed to get operator socket"))

	err := c.node.Start(context.Background())
	assert.NoError(t, err)
}

func TestNodeStartOperatorIDMatch(t *testing.T) {
	c := newComponents(t, op0)
	c.node.Config.RegisterNodeAtStart = true
	c.node.Config.EthClientConfig = geth.EthClientConfig{
		RPCURLs:          []string{"http://localhost:8545"},
		PrivateKeyString: privateKey,
		NumConfirmations: 1,
	}
	c.tx.On("GetRegisteredQuorumIdsForOperator", mock.Anything).Return([]core.QuorumID{}, nil)
	c.tx.On("GetOperatorSetParams", mock.Anything, mock.Anything).Return(&core.OperatorSetParam{
		MaxOperatorCount:         uint32(4),
		ChurnBIPsOfOperatorStake: uint16(1000),
		ChurnBIPsOfTotalStake:    uint16(10),
	}, nil)
	c.tx.On("GetNumberOfRegisteredOperatorForQuorum", mock.Anything, mock.Anything).Return(uint32(0), nil)
	c.tx.On("RegisterOperator", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	c.tx.On("OperatorAddressToID", mock.Anything).Return(core.OperatorID(op0), nil)

	err := c.node.Start(context.Background())
	assert.NoError(t, err)
}

func TestNodeStartOperatorIDDoesNotMatch(t *testing.T) {
	c := newComponents(t, op0)
	c.node.Config.RegisterNodeAtStart = true
	c.node.Config.EthClientConfig = geth.EthClientConfig{
		RPCURLs:          []string{"http://localhost:8545"},
		PrivateKeyString: privateKey,
		NumConfirmations: 1,
	}
	c.tx.On("GetRegisteredQuorumIdsForOperator", mock.Anything).Return([]core.QuorumID{}, nil)
	c.tx.On("GetOperatorSetParams", mock.Anything, mock.Anything).Return(&core.OperatorSetParam{
		MaxOperatorCount:         uint32(4),
		ChurnBIPsOfOperatorStake: uint16(1000),
		ChurnBIPsOfTotalStake:    uint16(10),
	}, nil)
	c.tx.On("GetNumberOfRegisteredOperatorForQuorum", mock.Anything, mock.Anything).Return(uint32(0), nil)
	c.tx.On("RegisterOperator", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	c.tx.On("OperatorAddressToID", mock.Anything).Return(core.OperatorID{1}, nil)

	err := c.node.Start(context.Background())
	assert.ErrorContains(t, err, "operator ID mismatch")
}

func TestGetReachabilityURL(t *testing.T) {
	v1CheckPath := "api/v1/operators-info/port-check"
	url, err := node.GetReachabilityURL("https://dataapi.eigenda.xyz/", v1CheckPath, "123123123")
	assert.NoError(t, err)
	assert.Equal(t, "https://dataapi.eigenda.xyz/api/v1/operators-info/port-check?operator_id=123123123", url)

	v2CheckPath := "api/v2/operators/liveness"
	url, err = node.GetReachabilityURL("https://dataapi.eigenda.xyz", v2CheckPath, "123123123")
	assert.NoError(t, err)
	assert.Equal(t, "https://dataapi.eigenda.xyz/api/v2/operators/liveness?operator_id=123123123", url)
}
