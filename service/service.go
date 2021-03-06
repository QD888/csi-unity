package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/dell/csi-unity/core"
	"github.com/dell/csi-unity/service/utils"
	"github.com/dell/gobrick"
	"github.com/dell/goiscsi"
	"github.com/dell/gounity"
	"github.com/fsnotify/fsnotify"
	"github.com/rexray/gocsi"
	csictx "github.com/rexray/gocsi/context"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"io/ioutil"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// Name is the name of the Unity CSI.

	// VendorVersion is the version of this Unity CSI.
	VendorVersion = "0.0.0"

	//Tcp dial default timeout in Milliseconds
	TcpDialTimeout = 1000

	IScsiPort = "3260"
)

var Name string
var DriverConfig string

//To maintain runid for Non debug mode. Note: CSI will not generate runid if CSI_DEBUG=false
var runid int64

// Manifest is the SP's manifest.
var Manifest = map[string]string{
	"url":    "http://github.com/dell/csi-unity",
	"semver": core.SemVer,
	"commit": core.CommitSha32,
	"formed": core.CommitTime.Format(time.RFC1123),
}

//To parse the secret json file
type StorageArrayList struct {
	StorageArrayList []StorageArrayConfig `json:"storageArrayList"`
}

type StorageArrayConfig struct {
	ArrayId        string `json:"arrayId"`
	Username       string `json:"username"`
	Password       string `json:"password"`
	RestGateway    string `json:"restGateway"`
	Insecure       bool   `json:"insecure, omitempty"`
	IsDefaultArray bool   `json:"isDefaultArray, omitempty"`
	IsProbeSuccess bool
	IsHostAdded    bool
	UnityClient    *gounity.Client
}

// Service is a CSI SP and idempotency.Provider.
type Service interface {
	csi.ControllerServer
	csi.IdentityServer
	csi.NodeServer
	BeforeServe(context.Context, *gocsi.StoragePlugin, net.Listener) error
}

// Opts defines service configuration options.
type Opts struct {
	NodeName                      string
	LongNodeName                  string
	Chroot                        string
	Thick                         bool
	AutoProbe                     bool
	PvtMountDir                   string
	Debug                         bool
	SyncNodeInfoTimeInterval      int
	EnvEphemeralStagingTargetPath string
}

type service struct {
	opts           Opts
	arrays         *sync.Map
	mode           string
	iscsiClient    goiscsi.ISCSIinterface
	fcConnector    fcConnector //gobrick connectors
	iscsiConnector iSCSIConnector
}

type iSCSIConnector interface {
	ConnectVolume(ctx context.Context, info gobrick.ISCSIVolumeInfo) (gobrick.Device, error)
	DisconnectVolumeByDeviceName(ctx context.Context, name string) error
	GetInitiatorName(ctx context.Context) ([]string, error)
}

type fcConnector interface {
	ConnectVolume(ctx context.Context, info gobrick.FCVolumeInfo) (gobrick.Device, error)
	DisconnectVolumeByDeviceName(ctx context.Context, name string) error
	GetInitiatorPorts(ctx context.Context) ([]string, error)
}

// New returns a new CSI Service.
func New() Service {
	return &service{}
}

//To display the StorageArrayConfig content
func (s StorageArrayConfig) String() string {
	return fmt.Sprintf("ArrayID: %s, Username: %s, RestGateway: %s, Insecure: %v, IsDefaultArray:%v, IsProbeSuccess:%v, IsHostAdded:%v",
		s.ArrayId, s.Username, s.RestGateway, s.Insecure, s.IsDefaultArray, s.IsProbeSuccess, s.IsHostAdded)
}

// BeforeServe allows the SP to participate in the startup
// sequence. This function is invoked directly before the
// gRPC server is created, giving the callback the ability to
// modify the SP's interceptors, server options, or prevent the
// server from starting by returning a non-nil error.
func (s *service) BeforeServe(
	ctx context.Context, sp *gocsi.StoragePlugin, lis net.Listener) error {
	ctx, log := setRunIdContext(ctx, "start")
	var err error
	defer func() {
		fields := map[string]interface{}{
			"nodename":  s.opts.NodeName,
			"autoprobe": s.opts.AutoProbe,
			"mode":      s.mode,
		}
		log.WithFields(fields).Infof("configured %s", Name)
	}()

	// Get the SP's operating mode.
	s.mode = csictx.Getenv(ctx, gocsi.EnvVarMode)
	log.Info("Driver Mode:", s.mode)

	opts := Opts{}
	if name, ok := csictx.LookupEnv(ctx, gocsi.EnvVarDebug); ok {
		opts.Debug, _ = strconv.ParseBool(name)
	}
	if name, ok := csictx.LookupEnv(ctx, EnvNodeName); ok {
		log.Info("X_CSI_UNITY_NODENAME:", name)
		opts.LongNodeName = name
		shortHostName := strings.Split(name, ".")[0]
		opts.NodeName = shortHostName
	}

	opts.SyncNodeInfoTimeInterval = 15
	if syncNodeInfoTimeInterval, ok := csictx.LookupEnv(ctx, SyncNodeInfoTimeInterval); ok {
		opts.SyncNodeInfoTimeInterval, err = strconv.Atoi(syncNodeInfoTimeInterval)
		log.Debugf("SyncNodeInfoTimeInterval %d", opts.SyncNodeInfoTimeInterval)
		if err != nil {
			opts.SyncNodeInfoTimeInterval = 15
		}
	}

	// pb parses an environment variable into a boolean value. If an error
	// is encountered, default is set to false, and error is logged
	pb := func(n string) bool {
		if v, ok := csictx.LookupEnv(ctx, n); ok {
			b, err := strconv.ParseBool(v)
			if err != nil {
				log.WithField(n, v).Debug(
					"invalid boolean value. defaulting to false")
				return false
			}
			return b
		}
		return false
	}

	opts.AutoProbe = pb(EnvAutoProbe)

	//Global mount directory will be used to node unstage volumes mounted via CSI-Unity v1.0 or v1.1
	if pvtmountDir, ok := csictx.LookupEnv(ctx, EnvPvtMountDir); ok {
		opts.PvtMountDir = pvtmountDir
	}

	if ephemeralStagePath, ok := csictx.LookupEnv(ctx, EnvEphemeralStagingPath); ok {
		opts.EnvEphemeralStagingTargetPath = ephemeralStagePath
	}

	// setup the iscsi client
	iscsiOpts := make(map[string]string, 0)
	if chroot, ok := csictx.LookupEnv(ctx, EnvISCSIChroot); ok {
		iscsiOpts[goiscsi.ChrootDirectory] = chroot
		opts.Chroot = chroot
	}
	s.iscsiClient = goiscsi.NewLinuxISCSI(iscsiOpts)

	s.opts = opts
	//Update the storage array list
	runid := fmt.Sprintf("config-%d", 0)
	ctx, log = setRunIdContext(ctx, runid)
	s.arrays = new(sync.Map)
	err = s.syncDriverConfig(ctx)
	if err != nil {
		return err
	}
	syncNodeInfoChan = make(chan bool)
	//Dynamically load the config
	go s.loadDynamicConfig(ctx, DriverConfig)

	//Add node information to hosts
	if s.mode == "node" {
		//Get Host Name
		if s.opts.NodeName == "" {
			return status.Error(codes.InvalidArgument, "'Node Name' has not been configured. Set environment variable X_CSI_UNITY_NODENAME")
		}

		go s.syncNodeInfoRoutine(ctx)
		syncNodeInfoChan <- true
	}

	return nil
}

//Get storage array from sync Map
func (s *service) getStorageArray(arrayID string) *StorageArrayConfig {
	if a, ok := s.arrays.Load(arrayID); ok {
		return a.(*StorageArrayConfig)
	}
	return nil
}

//Returns the size of arrays
func (s *service) getStorageArrayLength() (length int) {
	length = 0
	s.arrays.Range(func(_, _ interface{}) bool {
		length++
		return true
	})
	return
}

//Get storage array list from sync Map
func (s *service) getStorageArrayList() []*StorageArrayConfig {
	list := make([]*StorageArrayConfig, 0)
	s.arrays.Range(func(key interface{}, value interface{}) bool {
		list = append(list, value.(*StorageArrayConfig))
		return true
	})
	return list
}

// To get the UnityClient for a specific array
func (s *service) getUnityClient(ctx context.Context, arrayID string) (*gounity.Client, error) {
	_, _, rid := GetRunidLog(ctx)
	if s.getStorageArrayLength() == 0 {
		return nil, status.Error(codes.InvalidArgument, utils.GetMessageWithRunID(rid, "Invalid driver csi-driver configuration provided. At least one array should present or invalid json format. "))
	}

	array := s.getStorageArray(arrayID)
	if array != nil && array.UnityClient != nil {
		return array.UnityClient, nil
	} else {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("Unity client not found for array %s", arrayID))
	}
}

//return volumeid from csi volume context
func getVolumeIdFromVolumeContext(contextVolId string) string {
	if contextVolId == "" {
		return ""
	}
	tokens := strings.Split(contextVolId, "-")
	if len(tokens) == 1 {
		// Only one token found, which means volume created using csi-unity v1.0 and v1.1
		return tokens[0]
	} else if len(tokens) >= 4 {
		return tokens[len(tokens)-1]
	}
	return ""
}

//@Below method is unused. So remove.
func (s *service) getArrayIdFromVolumeContext(contextVolId string) (string, error) {
	if contextVolId == "" {
		return "", errors.New("volume context id should not be empty ")
	}
	tokens := strings.Split(contextVolId, "-")
	if len(tokens) == 1 {
		// Only one token found, which means volume created using csi-unity v1.0 and v1.1. So return default array
		for _, array := range s.getStorageArrayList() {
			if array.IsDefaultArray {
				return array.ArrayId, nil
			}
		}
		return "", errors.New("no default array found in the csi-unity driver configuration")
	} else if len(tokens) >= 4 {
		return tokens[len(tokens)-2], nil
	}
	return "", errors.New("invalid volume context id or no default array found in the csi-unity driver configuration")
}

var watcher *fsnotify.Watcher

func (s *service) loadDynamicConfig(ctx context.Context, configFile string) error {
	i := 1
	runid := fmt.Sprintf("config-%d", i)
	ctx, log := setRunIdContext(ctx, runid)

	log.Info("Dynamic config load goroutine invoked")
	watcher, _ := fsnotify.NewWatcher()
	defer watcher.Close()

	parentFolder, _ := filepath.Abs(filepath.Dir(configFile))
	log.Debug("Config folder:", parentFolder)
	done := make(chan bool)
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Create == fsnotify.Create && event.Name == parentFolder+"/..data" {
					log.Infof("****************Driver config file modified. Loading the config file:%s****************", event.Name)
					err := s.syncDriverConfig(ctx)
					if err != nil {
						log.Debug("Driver configuration array length:", s.getStorageArrayLength())
						log.Error("Invalid configuration in secret.json. Error:", err)
						//return
					}
					if s.mode == "node" {
						syncNodeInfoChan <- true
					}
					i++
				}
				runid = fmt.Sprintf("config-%d", i)
				ctx, log = setRunIdContext(ctx, runid)
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Error("Driver config load error:", err)
			}
		}
	}()
	err := watcher.Add(parentFolder)
	if err != nil {
		log.Error("Unable to add file watcher for folder ", parentFolder)
		return err
	}
	<-done
	return nil
}

//return protocol from csi volume context
func (s *service) getProtocolFromVolumeContext(contextVolId string) (string, error) {
	if contextVolId == "" {
		return "", errors.New("volume context id should not be empty ")
	}
	tokens := strings.Split(contextVolId, "-")
	if len(tokens) == 1 {
		// Only one token found, which means volume created using csi-unity v1.0 and v1.1. So return Unknown protocol
		return ProtocolUnknown, nil
	} else if len(tokens) >= 4 {
		return tokens[len(tokens)-3], nil
	}
	return "", errors.New("invalid volume context id")
}

var syncMutex sync.Mutex

//Reads the credentials from secrets and initialize all arrays.
func (s *service) syncDriverConfig(ctx context.Context) error {
	ctx, log, _ := GetRunidLog(ctx)
	log.Info("*************Synchronizing driver config**************")
	syncMutex.Lock()
	defer syncMutex.Unlock()
	s.arrays.Range(func(key interface{}, value interface{}) bool {
		s.arrays.Delete(key)
		return true
	})
	configBytes, err := ioutil.ReadFile(DriverConfig)
	if err != nil {
		return errors.New(fmt.Sprintf("File ('%s') error: %v", DriverConfig, err))
	}

	if string(configBytes) != "" {
		jsonConfig := new(StorageArrayList)
		err := json.Unmarshal(configBytes, &jsonConfig)
		if err != nil {
			return errors.New(fmt.Sprintf("Unable to parse the credentials [%v]", err))
		}

		if len(jsonConfig.StorageArrayList) == 0 {
			return errors.New("Arrays details are not provided in unity-creds secret")
		}

		s.arrays.Range(func(key interface{}, value interface{}) bool {
			s.arrays.Delete(key)
			return true
		})
		var noOfDefaultArrays int
		for i, config := range jsonConfig.StorageArrayList {
			if config.ArrayId == "" {
				return errors.New(fmt.Sprintf("invalid value for ArrayID at index [%d]", i))
			}
			if config.Username == "" {
				return errors.New(fmt.Sprintf("invalid value for Username at index [%d]", i))
			}
			if config.Password == "" {
				return errors.New(fmt.Sprintf("invalid value for Password at index [%d]", i))
			}
			if config.RestGateway == "" {
				return errors.New(fmt.Sprintf("invalid value for RestGateway at index [%d]", i))
			}

			config.ArrayId = strings.ToLower(config.ArrayId)
			unityClient, err := gounity.NewClientWithArgs(ctx, config.RestGateway, config.Insecure)
			if err != nil {
				return errors.New(fmt.Sprintf("unable to initialize the Unity client [%v]", err))
			}
			config.UnityClient = unityClient

			copy := StorageArrayConfig{}
			copy = config

			if _, ok := s.arrays.Load(config.ArrayId); ok {
				return errors.New(fmt.Sprintf("Duplicate ArrayID [%s] found in storageArrayList parameter", config.ArrayId))
			} else {
				s.arrays.Store(config.ArrayId, &copy)
			}

			fields := logrus.Fields{
				"RestGateway":    config.RestGateway,
				"ArrayId":        config.ArrayId,
				"username":       config.Username,
				"password":       "*******",
				"Insecure":       config.Insecure,
				"IsDefaultArray": config.IsDefaultArray,
			}
			logrus.WithFields(fields).Infof("configured %s", Name)

			if config.IsDefaultArray {
				noOfDefaultArrays++
			}

			if noOfDefaultArrays > 1 {
				return errors.New(fmt.Sprintf("'isDefaultArray' parameter located in multiple places ArrayId: %s. 'isDefaultArray' parameter should present only once in the storageArrayList.", config.ArrayId))
			}
		}
	} else {
		return errors.New("Arrays details are not provided in unity-creds secret")
	}

	return nil
}

//Set arraysId in log messages and re-initialize the context
func setArrayIdContext(ctx context.Context, arrayId string) (context.Context, *logrus.Entry) {
	return setLogFieldsInContext(ctx, arrayId, utils.ARRAYID)
}

//Set arraysId in log messages and re-initialize the context
func setRunIdContext(ctx context.Context, runId string) (context.Context, *logrus.Entry) {
	return setLogFieldsInContext(ctx, runId, utils.RUNID)
}

var logMutex sync.Mutex

//Common method to get log and context
func setLogFieldsInContext(ctx context.Context, logId string, logType string) (context.Context, *logrus.Entry) {
	logMutex.Lock()
	defer logMutex.Unlock()

	fields := logrus.Fields{}
	fields, ok := ctx.Value(utils.LogFields).(logrus.Fields)
	if !ok {
		fields = logrus.Fields{}
	}
	if fields == nil {
		fields = logrus.Fields{}
	}
	fields[logType] = logId
	ulog, ok := ctx.Value(utils.UnityLogger).(*logrus.Entry)
	if !ok {
		ulog = utils.GetLogger().WithFields(fields)
	}
	ulog = ulog.WithFields(fields)
	ctx = context.WithValue(ctx, utils.UnityLogger, ulog)
	ctx = context.WithValue(ctx, utils.LogFields, fields)
	return ctx, ulog
}

var syncNodeLogCount int32
var syncConfigLogCount int32

//Increment run id log
func incrementLogId(ctx context.Context, runidPrefix string) (context.Context, *logrus.Entry) {
	if runidPrefix == "node" {
		runid := fmt.Sprintf("%s-%d", runidPrefix, syncNodeLogCount)
		atomic.AddInt32(&syncNodeLogCount, 1)
		return setRunIdContext(ctx, runid)
	} else if runidPrefix == "config" {
		runid := fmt.Sprintf("%s-%d", runidPrefix, syncConfigLogCount)
		atomic.AddInt32(&syncConfigLogCount, 1)
		return setRunIdContext(ctx, runid)
	}
	return nil, nil
}

func GetRunidLog(ctx context.Context) (context.Context, *logrus.Entry, string) {
	var rid string
	fields := logrus.Fields{}
	if ctx == nil {
		return ctx, utils.GetLogger().WithFields(fields), rid
	}

	headers, ok := metadata.FromIncomingContext(ctx)
	if ok {
		reqid, ok := headers[csictx.RequestIDKey]
		if ok && len(reqid) > 0 {
			rid = reqid[0]
		} else {
			atomic.AddInt64(&runid, 1)
			rid = fmt.Sprintf("%d", runid)
		}
	}

	fields, _ = ctx.Value(utils.LogFields).(logrus.Fields)
	if fields == nil {
		fields = logrus.Fields{}
	}

	if ok {
		fields[utils.RUNID] = rid
	}

	logMutex.Lock()
	defer logMutex.Unlock()
	l := utils.GetLogger()
	log := l.WithFields(fields)
	ctx = context.WithValue(ctx, utils.UnityLogger, log)
	ctx = context.WithValue(ctx, utils.LogFields, fields)
	return ctx, log, rid
}

func getLogFields(ctx context.Context) logrus.Fields {
	fields := logrus.Fields{}
	if ctx == nil {
		return fields
	}
	fields, ok := ctx.Value(utils.LogFields).(logrus.Fields)
	if !ok {
		fields = logrus.Fields{}
	}

	csiReqID, ok := ctx.Value(csictx.RequestIDKey).(string)
	if !ok {
		return fields
	}
	fields[utils.RUNID] = csiReqID
	return fields
}

func (s *service) initISCSIConnector(chroot string) {
	if s.iscsiConnector == nil {
		setupGobrick(s)
		s.iscsiConnector = gobrick.NewISCSIConnector(
			gobrick.ISCSIConnectorParams{Chroot: chroot})
	}
}

func (s *service) initFCConnector(chroot string) {
	if s.fcConnector == nil {
		setupGobrick(s)
		s.fcConnector = gobrick.NewFCConnector(
			gobrick.FCConnectorParams{Chroot: chroot})
	}
}

func setupGobrick(srv *service) {
	gobrick.SetLogger(&customLogger{})
	gobrick.SetTracer(&emptyTracer{})
}

type emptyTracer struct{}

func (dl *emptyTracer) Trace(ctx context.Context, format string, args ...interface{}) {
}

type customLogger struct{}

func (lg *customLogger) Info(ctx context.Context, format string, args ...interface{}) {
	log := utils.GetLogger()
	log.WithFields(getLogFields(ctx)).Infof(format, args...)
}
func (lg *customLogger) Debug(ctx context.Context, format string, args ...interface{}) {
	log := utils.GetLogger()
	log.WithFields(getLogFields(ctx)).Debugf(format, args...)
}
func (lg *customLogger) Error(ctx context.Context, format string, args ...interface{}) {
	log := utils.GetLogger()
	log.WithFields(getLogFields(ctx)).Errorf(format, args...)
}

func (s *service) requireProbe(ctx context.Context, arrayId string) error {
	rid, log := utils.GetRunidAndLogger(ctx)
	if !s.opts.AutoProbe {
		return status.Error(codes.FailedPrecondition, utils.GetMessageWithRunID(rid, "Controller Service has not been probed"))
	}
	log.Debug("Probing controller service automatically")
	if err := s.controllerProbe(ctx, arrayId); err != nil {
		return status.Error(codes.FailedPrecondition, utils.GetMessageWithRunID(rid, "failed to probe/init plugin: %s", err.Error()))
	}
	return nil
}

func singleArrayProbe(ctx context.Context, probeType string, array *StorageArrayConfig) error {
	rid, log := utils.GetRunidAndLogger(ctx)
	ctx, log = setArrayIdContext(ctx, array.ArrayId)
	if array.UnityClient.GetToken() == "" {
		err := array.UnityClient.Authenticate(ctx, &gounity.ConfigConnect{
			Endpoint: array.RestGateway,
			Username: array.Username,
			Password: array.Password,
		})
		if err != nil {
			log.Errorf("Unity authentication failed for array %s error: %v", array.ArrayId, err)
			if e, ok := status.FromError(err); ok {
				if e.Code() == codes.Unauthenticated {
					array.IsProbeSuccess = false
					return status.Error(codes.FailedPrecondition, utils.GetMessageWithRunID(rid, "Unable to login to Unity. Error: %s", err.Error()))
				}
			}
			array.IsProbeSuccess = false
			return status.Error(codes.FailedPrecondition, utils.GetMessageWithRunID(rid, "Unable to login to Unity. Verify hostname/IP Address of unity. Error: %s", err.Error()))
		} else {
			array.IsProbeSuccess = true
			log.Debugf("%s Probe Success", probeType)
			return nil
		}
	}
	return nil
}

func (s *service) probe(ctx context.Context, probeType string, arrayId string) error {
	rid, log := utils.GetRunidAndLogger(ctx)
	log.Debugf("Inside %s Probe", probeType)
	if arrayId != "" {
		if array := s.getStorageArray(arrayId); array != nil {
			return singleArrayProbe(ctx, probeType, array)
		}
	} else {
		log.Debug("Probing all arrays")
		atleastOneArraySuccess := false
		for _, array := range s.getStorageArrayList() {
			err := singleArrayProbe(ctx, probeType, array)
			if err == nil {
				atleastOneArraySuccess = true
				break
			} else {
				log.Errorf("Probe failed for array %s error:%v", array, err)
			}
		}

		if !atleastOneArraySuccess {
			return status.Error(codes.FailedPrecondition, utils.GetMessageWithRunID(rid, "All unity arrays are not working. Could not proceed further"))
		}
	}
	log.Infof("%s Probe Success", probeType)
	return nil
}

func (s *service) validateAndGetResourceDetails(ctx context.Context, resourceContextId string, resourceType resourceType) (resourceId, protocol, arrayId string, unity *gounity.Client, err error) {
	ctx, _, rid := GetRunidLog(ctx)
	if s.getStorageArrayLength() == 0 {
		return "", "", "", nil, status.Error(codes.InvalidArgument, utils.GetMessageWithRunID(rid, "Invalid driver csi-driver configuration provided. At least one array should present or invalid json format. "))
	}
	resourceId = getVolumeIdFromVolumeContext(resourceContextId)
	if resourceId == "" {
		return "", "", "", nil, status.Error(codes.InvalidArgument, utils.GetMessageWithRunID(rid, "%sId can't be empty.", resourceType))
	}
	arrayId, err = s.getArrayIdFromVolumeContext(resourceContextId)
	if err != nil {
		return "", "", "", nil, status.Error(codes.InvalidArgument, utils.GetMessageWithRunID(rid, "[%s] [%s] error:[%v]", resourceType, resourceId, err))
	}

	protocol, err = s.getProtocolFromVolumeContext(resourceContextId)
	if err != nil {
		return "", "", "", nil, status.Error(codes.InvalidArgument, utils.GetMessageWithRunID(rid, "[%s] [%s] error:[%v]", resourceType, resourceId, err))
	}

	unity, err = s.getUnityClient(ctx, arrayId)
	if err != nil {
		return "", "", "", nil, err
	}
	return
}
