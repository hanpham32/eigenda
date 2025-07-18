package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Layr-Labs/eigenda/common"
	caws "github.com/Layr-Labs/eigenda/common/aws"
	"github.com/Layr-Labs/eigenda/common/geth"
	relayreg "github.com/Layr-Labs/eigenda/contracts/bindings/EigenDARelayRegistry"
	eigendasrvmg "github.com/Layr-Labs/eigenda/contracts/bindings/EigenDAServiceManager"
	thresholdreg "github.com/Layr-Labs/eigenda/contracts/bindings/EigenDAThresholdRegistry"
	"github.com/Layr-Labs/eigenda/core/eth"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Layr-Labs/eigenda/core"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	gcommon "github.com/ethereum/go-ethereum/common"
)

const (
	churnerImage   = "ghcr.io/layr-labs/eigenda/churner:local"
	disImage       = "ghcr.io/layr-labs/eigenda/disperser:local"
	encoderImage   = "ghcr.io/layr-labs/eigenda/encoder:local"
	batcherImage   = "ghcr.io/layr-labs/eigenda/batcher:local"
	nodeImage      = "ghcr.io/layr-labs/eigenda/node:local"
	retrieverImage = "ghcr.io/layr-labs/eigenda/retriever:local"
	relayImage     = "ghcr.io/layr-labs/eigenda/relay:local"
)

// getKeyString retrieves a ECDSA private key string for a given Ethereum account
func (env *Config) getKeyString(name string) string {
	key, _ := env.getKey(name)
	keyInt, ok := new(big.Int).SetString(key, 0)
	if !ok {
		log.Panicf("Error: could not parse key %s", key)
	}
	return keyInt.String()
}

// generateV1CertVerifierDeployConfig generates the input config used for deploying the V1 CertVerifier
// NOTE: this will be killed in the future with eventual deprecation of V1
func (env *Config) generateV1CertVerifierDeployConfig(ethClient common.EthClient) V1CertVerifierDeployConfig {
	config := V1CertVerifierDeployConfig{
		ServiceManager:                env.EigenDA.ServiceManager,
		RequiredQuorums:               []uint32{0, 1},
		RequiredAdversarialThresholds: []uint32{33, 33},
		RequiredConfirmationQuorums:   []uint32{55, 55},
	}

	return config
}

// generateEigenDADeployConfig generates input config fed into SetUpEigenDA.s.sol foundry script
func (env *Config) generateEigenDADeployConfig() EigenDADeployConfig {

	operators := make([]string, 0)
	stakers := make([]string, 0)
	maxOperatorCount := env.Services.Counts.NumMaxOperatorCount

	numStrategies := len(env.Services.Stakes)
	total := make([]float32, numStrategies)
	stakes := make([][]string, numStrategies)

	for quorum, stake := range env.Services.Stakes {
		for _, s := range stake.Distribution {
			total[quorum] += s
		}
	}

	for quorum := 0; quorum < numStrategies; quorum++ {
		stakes[quorum] = make([]string, len(env.Services.Stakes[quorum].Distribution))
		for ind, stake := range env.Services.Stakes[quorum].Distribution {
			stakes[quorum][ind] = strconv.FormatFloat(float64(stake/total[quorum]*env.Services.Stakes[quorum].Total), 'f', 0, 32)
		}
	}

	for i := 0; i < len(env.Services.Stakes[0].Distribution); i++ {
		stakerName := fmt.Sprintf("staker%d", i)
		operatorName := fmt.Sprintf("opr%d", i)

		stakers = append(stakers, env.getKeyString(stakerName))
		operators = append(operators, env.getKeyString(operatorName))
	}

	config := EigenDADeployConfig{
		UseDefaults:         true,
		NumStrategies:       numStrategies,
		MaxOperatorCount:    maxOperatorCount,
		StakerPrivateKeys:   stakers,
		StakerTokenAmounts:  stakes,
		OperatorPrivateKeys: operators,
		ConfirmerPrivateKey: env.getKeyString("batcher0"),
	}

	return config

}

// deployEigenDAContracts deploys EigenDA core system and peripheral contracts on local anvil chain
func (env *Config) deployEigenDAContracts() {
	log.Print("Deploy the EigenDA and EigenLayer contracts")

	// get deployer
	deployer, ok := env.GetDeployer(env.EigenDA.Deployer)
	if !ok {
		log.Panicf("Deployer improperly configured")
	}

	changeDirectory(filepath.Join(env.rootPath, "contracts"))

	eigendaDeployConfig := env.generateEigenDADeployConfig()
	data, err := json.Marshal(&eigendaDeployConfig)
	if err != nil {
		log.Panicf("Error: %s", err.Error())
	}
	writeFile("script/input/eigenda_deploy_config.json", data)

	execForgeScript("script/SetUpEigenDA.s.sol:SetupEigenDA", env.Pks.EcdsaMap[deployer.Name].PrivateKey, deployer, nil)

	//add relevant addresses to path
	data = readFile("script/output/eigenda_deploy_output.json")
	err = json.Unmarshal(data, &env.EigenDA)
	if err != nil {
		log.Panicf("Error: %s", err.Error())
	}

	logger, err := common.NewLogger(common.DefaultLoggerConfig())
	if err != nil {
		log.Panicf("Error: %s", err.Error())
	}

	ethClient, err := geth.NewClient(geth.EthClientConfig{
		RPCURLs:          []string{deployer.RPC},
		PrivateKeyString: env.Pks.EcdsaMap[deployer.Name].PrivateKey[2:],
		NumConfirmations: 0,
		NumRetries:       0,
	}, gcommon.Address{}, 0, logger)
	if err != nil {
		log.Panicf("Error: %s", err.Error())
	}

	certVerifierV1DeployCfg := env.generateV1CertVerifierDeployConfig(ethClient)
	data, err = json.Marshal(&certVerifierV1DeployCfg)
	if err != nil {
		log.Panicf("Error: %s", err.Error())
	}

	// NOTE: this is pretty janky and is a short-term solution until V1 contract usage
	//       can be deprecated.
	writeFile("script/deploy/certverifier/config/v1/inabox_deploy_config_v1.json", data)
	execForgeScript("script/deploy/certverifier/CertVerifierDeployerV1.s.sol:CertVerifierDeployerV1", env.Pks.EcdsaMap[deployer.Name].PrivateKey, deployer, []string{"--sig", "run(string, string)", "inabox_deploy_config_v1.json", "inabox_v1_deploy.json"})

	data = readFile("script/deploy/certverifier/output/inabox_v1_deploy.json")
	var verifierAddress struct{ EigenDACertVerifier string }
	err = json.Unmarshal(data, &verifierAddress)
	if err != nil {
		log.Panicf("Error: %s", err.Error())
	}
	env.EigenDAV1CertVerifier = verifierAddress.EigenDACertVerifier
}

// Deploys a EigenDA experiment
// TODO: Figure out what necessitates experiment nomenclature
func (env *Config) DeployExperiment() {
	changeDirectory(filepath.Join(env.rootPath, "inabox"))
	defer env.SaveTestConfig()

	log.Print("Deploying experiment...")

	// Log to file
	f, err := os.OpenFile(filepath.Join(env.Path, "deploy.log"), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Panicf("error opening file: %v", err)
	}
	defer core.CloseLogOnError(f, f.Name(), nil)
	log.SetOutput(io.MultiWriter(os.Stdout, f))

	// Create a new experiment and deploy the contracts

	err = env.loadPrivateKeys()
	if err != nil {
		log.Panicf("could not load private keys: %v", err)
	}

	if env.EigenDA.Deployer != "" && !env.IsEigenDADeployed() {
		fmt.Println("Deploying EigenDA")
		env.deployEigenDAContracts()
	}

	if deployer, ok := env.GetDeployer(env.EigenDA.Deployer); ok && deployer.DeploySubgraphs {
		startBlock := GetLatestBlockNumber(env.Deployers[0].RPC)
		env.deploySubgraphs(startBlock)
	}

	// Ideally these should be set in GenerateAllVariables, but they need to be used in GenerateDisperserKeypair
	// which is called before GenerateAllVariables
	env.localstackEndpoint = "http://localhost:4570"
	env.localstackRegion = "us-east-1"

	fmt.Println("Generating disperser keypair")
	err = env.GenerateDisperserKeypair()
	if err != nil {
		log.Panicf("could not generate disperser keypair: %v", err)
	}

	fmt.Println("Generating variables")
	env.GenerateAllVariables()

	fmt.Println("Test environment has successfully deployed!")
}

// GenerateDisperserKeypair generates a disperser keypair using AWS KMS.
func (env *Config) GenerateDisperserKeypair() error {

	// Generate a keypair in AWS KMS

	keyManager := kms.New(kms.Options{
		Region:       env.localstackRegion,
		BaseEndpoint: aws.String(env.localstackEndpoint),
	})

	createKeyOutput, err := keyManager.CreateKey(context.Background(), &kms.CreateKeyInput{
		KeySpec:  types.KeySpecEccSecgP256k1,
		KeyUsage: types.KeyUsageTypeSignVerify,
	})
	if err != nil {
		if strings.Contains(err.Error(), "connect: connection refused") {
			log.Printf("Unable to reach local stack, skipping disperser keypair generation. Error: %v", err)
			err = nil
		}
		return err
	}

	env.DisperserKMSKeyID = *createKeyOutput.KeyMetadata.KeyId

	// Load the public key and convert it to an Ethereum address

	key, err := caws.LoadPublicKeyKMS(context.Background(), keyManager, env.DisperserKMSKeyID)
	if err != nil {
		return fmt.Errorf("could not load public key: %v", err)
	}

	env.DisperserAddress = crypto.PubkeyToAddress(*key)
	log.Printf("Generated disperser keypair: key ID: %s, address: %s",
		env.DisperserKMSKeyID, env.DisperserAddress.Hex())

	return nil
}

// RegisterDisperserKeypair registers the disperser's public key on-chain.
func (env *Config) RegisterDisperserKeypair(ethClient common.EthClient) error {

	// Write the disperser's public key to on-chain storage

	loggerConfig := common.DefaultLoggerConfig()
	logger, err := common.NewLogger(loggerConfig)
	if err != nil {
		return fmt.Errorf("could not create logger: %v", err)
	}

	writer, err := eth.NewWriter(
		logger,
		ethClient,
		env.EigenDA.EigenDADirectory,
		env.EigenDA.OperatorStateRetriever,
		env.EigenDA.ServiceManager,
	)
	if err != nil {
		return fmt.Errorf("could not create writer: %v", err)
	}

	err = writer.SetDisperserAddress(context.Background(), env.DisperserAddress)
	if err != nil {
		return fmt.Errorf("could not set disperser address: %v", err)
	}

	// Read the disperser's public key from on-chain storage to verify it was written correctly

	retryTimeout := time.Now().Add(1 * time.Minute)
	ticker := time.NewTicker(1 * time.Second)

	for time.Now().Before(retryTimeout) {
		address, err := writer.GetDisperserAddress(context.Background(), 0)
		if err != nil {
			logger.Warnf("could not get disperser address: %v", err)
		} else {
			if address != env.DisperserAddress {
				return fmt.Errorf("expected disperser address %s, got %s", env.DisperserAddress, address)
			}
			return nil
		}

		<-ticker.C
	}

	return fmt.Errorf("timed out waiting for disperser address to be set")
}

// RegisterBlobVersionAndRelays initializes blob versions in ThresholdRegistry contract
// and relays in RelayRegistry contract
func (env *Config) RegisterBlobVersionAndRelays(ethClient common.EthClient) {
	dasmAddr := gcommon.HexToAddress(env.EigenDA.ServiceManager)
	contractEigenDAServiceManager, err := eigendasrvmg.NewContractEigenDAServiceManager(dasmAddr, ethClient)
	if err != nil {
		log.Panicf("Error: %s", err)
	}
	thresholdRegistryAddr, err := contractEigenDAServiceManager.EigenDAThresholdRegistry(&bind.CallOpts{})
	if err != nil {
		log.Panicf("Error: %s", err)
	}
	contractThresholdRegistry, err := thresholdreg.NewContractEigenDAThresholdRegistry(thresholdRegistryAddr, ethClient)
	if err != nil {
		log.Panicf("Error: %s", err)
	}
	opts, err := ethClient.GetNoSendTransactOpts()
	if err != nil {
		log.Panicf("Error: %s", err)
	}
	for _, blobVersionParam := range env.BlobVersionParams {
		txn, err := contractThresholdRegistry.AddVersionedBlobParams(opts, thresholdreg.EigenDATypesV1VersionedBlobParams{
			MaxNumOperators: blobVersionParam.MaxNumOperators,
			NumChunks:       blobVersionParam.NumChunks,
			CodingRate:      uint8(blobVersionParam.CodingRate),
		})
		if err != nil {
			log.Panicf("Error: %s", err)
		}
		err = ethClient.SendTransaction(context.Background(), txn)
		if err != nil {
			log.Panicf("Error: %s", err)
		}
	}

	relayAddr, err := contractEigenDAServiceManager.EigenDARelayRegistry(&bind.CallOpts{})
	if err != nil {
		log.Panicf("Error: %s", err)
	}
	contractRelayRegistry, err := relayreg.NewContractEigenDARelayRegistry(relayAddr, ethClient)
	if err != nil {
		log.Panicf("Error: %s", err)
	}

	ethAddr := ethClient.GetAccountAddress()
	for _, relayVars := range env.Relays {
		url := fmt.Sprintf("0.0.0.0:%s", relayVars.RELAY_GRPC_PORT)
		txn, err := contractRelayRegistry.AddRelayInfo(opts, relayreg.EigenDATypesV2RelayInfo{
			RelayAddress: ethAddr,
			RelayURL:     url,
		})
		if err != nil {
			log.Panicf("Error: %s", err)
		}
		err = ethClient.SendTransaction(context.Background(), txn)
		if err != nil {
			log.Panicf("Error: %s", err)
		}
	}
}

// TODO: Supply the test path to the runner utility
func (env *Config) StartBinaries() {
	changeDirectory(filepath.Join(env.rootPath, "inabox"))
	err := execCmd("./bin.sh", []string{"start-detached"}, []string{}, true)

	if err != nil {
		log.Panicf("Failed to start binaries. Err: %s", err)
	}
}

// TODO: Supply the test path to the runner utility
func (env *Config) StopBinaries() {
	changeDirectory(filepath.Join(env.rootPath, "inabox"))
	err := execCmd("./bin.sh", []string{"stop"}, []string{}, true)
	if err != nil {
		log.Panicf("Failed to stop binaries. Err: %s", err)
	}
}

func (env *Config) StartAnvil() {
	changeDirectory(filepath.Join(env.rootPath, "inabox"))
	err := execCmd("./bin.sh", []string{"start-anvil"}, []string{}, false) // printing output causes hang
	if err != nil {
		log.Panicf("Failed to start anvil. Err: %s", err)
	}
}

func (env *Config) StopAnvil() {
	changeDirectory(filepath.Join(env.rootPath, "inabox"))
	err := execCmd("./bin.sh", []string{"stop-anvil"}, []string{}, true)
	if err != nil {
		log.Panicf("Failed to stop anvil. Err: %s", err)
	}
}

func (env *Config) RunNodePluginBinary(operation string, operator OperatorVars) {
	changeDirectory(filepath.Join(env.rootPath, "inabox"))

	socket := string(core.MakeOperatorSocket(operator.NODE_HOSTNAME, operator.NODE_DISPERSAL_PORT, operator.NODE_RETRIEVAL_PORT, operator.NODE_V2_DISPERSAL_PORT, operator.NODE_V2_RETRIEVAL_PORT))

	envVars := []string{
		"NODE_OPERATION=" + operation,
		"NODE_ECDSA_KEY_FILE=" + operator.NODE_ECDSA_KEY_FILE,
		"NODE_BLS_KEY_FILE=" + operator.NODE_BLS_KEY_FILE,
		"NODE_ECDSA_KEY_PASSWORD=" + operator.NODE_ECDSA_KEY_PASSWORD,
		"NODE_BLS_KEY_PASSWORD=" + operator.NODE_BLS_KEY_PASSWORD,
		"NODE_SOCKET=" + socket,
		"NODE_QUORUM_ID_LIST=" + operator.NODE_QUORUM_ID_LIST,
		"NODE_CHAIN_RPC=" + operator.NODE_CHAIN_RPC,
		"NODE_EIGENDA_DIRECTORY=" + operator.NODE_EIGENDA_DIRECTORY,
		"NODE_BLS_OPERATOR_STATE_RETRIVER=" + operator.NODE_BLS_OPERATOR_STATE_RETRIVER,
		"NODE_EIGENDA_SERVICE_MANAGER=" + operator.NODE_EIGENDA_SERVICE_MANAGER,
		"NODE_CHURNER_URL=" + operator.NODE_CHURNER_URL,
		"NODE_NUM_CONFIRMATIONS=0",
	}

	err := execCmd("./node-plugin.sh", []string{}, envVars, true)

	if err != nil {
		log.Panicf("Failed to run node plugin. Err: %s", err)
	}
}
