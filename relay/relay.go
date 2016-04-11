package relay

import (
	"container/list"
	"encoding/json"
	"errors"
	log "github.com/Sirupsen/logrus"
	"github.com/operable/go-relay/relay/bus"
	"github.com/operable/go-relay/relay/config"
	"github.com/operable/go-relay/relay/engines"
	"github.com/operable/go-relay/relay/messages"
	"golang.org/x/net/context"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
	"time"
)

// ControlCommand are async signals sent to a running Relay
type ControlCommand int

// Allowed commands
const (
	RelayStop ControlCommand = iota
	RelayRestart
	RelayUpdateBundles
	RelayUpdateBundlesDone
)

// State describes Relay's various runtime states
type State int

// Runtime states
const (
	RelayStopped State = iota
	RelayStarting
	RelayUpdatingBundles
	RelayReady
)

var errorNoExecutionEngines = errors.New("Invalid Relay configuration detected. At least one execution engine must be enabled.")

// Worker pulls work items from a Relay's work queue
type Worker func(*Queue, sync.WaitGroup)

// Incoming request or directive
type Incoming struct {
	Relay       *Relay
	IsExecution bool
	Topic       string
	Payload     []byte
}

// Relay is a single instance of a Relay
type Relay struct {
	Config        *config.Config
	Bus           bus.MessageBus
	bundleLock    sync.RWMutex
	bundles       map[string]*config.Bundle
	bundlesHash   uint64
	announce      bool
	fetchedImages *list.List
	workQueue     *Queue
	worker        Worker
	refreshTimer  *time.Timer
	dockerTimer   *time.Timer
	hasStarted    bool
	coordinator   sync.WaitGroup
	control       chan ControlCommand
	state         State
}

// New creates a new Relay instance with the specified config
func New(relayConfig *config.Config) *Relay {
	return &Relay{
		Config:        relayConfig,
		bundles:       make(map[string]*config.Bundle),
		fetchedImages: list.New(),
		// Create work queue with some burstable capacity
		workQueue: NewQueue(relayConfig.MaxConcurrent * 2),
		control:   make(chan ControlCommand, 2),
		state:     RelayStopped,
	}
}

// Start initializes a Relay. Returns an error
// if execution engines or Docker config fails verification
func (r *Relay) Start(worker Worker) error {
	if err := r.verifyEnabledExecutionEngines(); err != nil {
		return err
	}
	if err := r.verifyDockerConfig(); err != nil {
		return err
	}
	r.state = RelayStarting
	r.startWorkers(worker)
	r.connectToCog()
	r.worker = worker
	go r.runLoop()
	return nil
}

// Stop a running relay
func (r *Relay) Stop() {
	if r.state != RelayStopped {
		r.stopTimers()
		if r.Bus != nil {
			r.Bus.Halt()
		}
		r.workQueue.Stop()
		r.control <- RelayStop
		r.coordinator.Wait()
		r.state = RelayStopped
	}
}

// UpdateBundles causes a Relay to ask Cog
// for its bundle assignments
func (r *Relay) UpdateBundles() bool {
	r.control <- RelayUpdateBundles
	return true
}

// FinishedUpdateBundles is used by worker processes to
// signal when the a bundle refresh is complete.
func (r *Relay) FinishedUpdateBundles() bool {
	if r.state != RelayUpdatingBundles {
		return false
	}
	r.control <- RelayUpdateBundlesDone
	return true
}

// GetBundle returns the named config.Bundle or nil
func (r *Relay) GetBundle(name string) *config.Bundle {
	r.bundleLock.RLock()
	defer r.bundleLock.RUnlock()
	return r.bundles[name]
}

// UpdateBundleList atomically replaces the existing master bundle list
// with a new one
func (r *Relay) UpdateBundleList(bundles map[string]*config.Bundle) {
	r.bundleLock.Lock()
	defer r.bundleLock.Unlock()
	newBundlesHash := computeBundleHash(bundles)
	if r.bundlesHash != newBundlesHash {
		r.bundles = bundles
		r.bundlesHash = newBundlesHash
		r.announce = true
	}
}

// BundleNames returns list of bundles known by a Relay
func (r *Relay) BundleNames() []string {
	r.bundleLock.RLock()
	defer r.bundleLock.RUnlock()
	bundleCount := len(r.bundles)
	retval := make([]string, bundleCount)
	i := 0
	for k := range r.bundles {
		retval[i] = k
		i++
	}
	return retval
}

func (r *Relay) startWorkers(worker Worker) {
	workerCount := r.Config.MaxConcurrent + 2
	for i := 0; i < workerCount; i++ {
		go func() {
			worker(r.workQueue, r.coordinator)
		}()
	}
	log.Infof("Started %d workers.", r.Config.MaxConcurrent)

}

func (r *Relay) connectToCog() error {
	// Handler func used for both message types
	handler := func(bus bus.MessageBus, topic string, payload []byte) {
		r.handleMessage(topic, payload)
	}

	// Connect to Cog
	handlers := bus.Handlers{
		CommandHandler:    handler,
		ExecutionHandler:  handler,
		DisconnectHandler: r.disconnected,
	}
	link, err := bus.NewLink(r.Config.ID, r.Config.Cog, handlers, r.coordinator)
	if err != nil {
		log.Errorf("Error connecting to Cog: %s.", err)
		return err
	}

	err = link.Run()
	if err != nil {
		log.Errorf("Error connecting to Cog: %s.", err)
		return err
	}
	log.Infof("Connected to Cog host %s.", r.Config.Cog.Host)
	r.Bus = link
	return nil
}

func (r *Relay) disconnected(err error) {
	log.Errorf("Relay %s disconnected due to error: %s.", r.Config.ID, err)
	r.control <- RelayRestart
}

func (r *Relay) handleMessage(topic string, payload []byte) {
	incoming := &Incoming{
		Relay:       r,
		Topic:       topic,
		IsExecution: strings.HasPrefix(topic, "/bot/commands/"),
		Payload:     payload,
	}
	ctx := context.WithValue(context.Background(), "incoming", incoming)
	r.workQueue.Enqueue(ctx)
}

func (r *Relay) verifyDockerConfig() error {
	if r.Config.DockerEnabled() == true {
		if err := engines.VerifyDockerConfig(r.Config.Docker); err != nil {
			log.Errorf("Error verifying Docker configuration: %s.", err)
			return err
		}
		log.Infof("Docker configuration verified.")
	} else {
		log.Infof("Docker support disabled.")
	}
	return nil
}

func (r *Relay) verifyEnabledExecutionEngines() error {
	if r.Config.DockerEnabled() == false && r.Config.NativeEnabled() == false {
		log.Errorf("%s", errorNoExecutionEngines)
		return errorNoExecutionEngines
	}
	if r.Config.DockerEnabled() == true {
		log.Info("Docker execution engine enabled.")
	}
	if r.Config.NativeEnabled() == true {
		log.Info("Native execution engine enabled.")
	}
	return nil
}

func (r *Relay) runLoop() {
	r.coordinator.Add(1)
	defer r.coordinator.Done()
	for {
		switch <-r.control {
		case RelayStop:
			return
		case RelayRestart:
			r.handleRestartCommand()
		case RelayUpdateBundles:
			r.handleUpdateBundlesCommand()
		case RelayUpdateBundlesDone:
			r.handleUpdateBundlesDone()
		}
	}
}

func (r *Relay) handleRestartCommand() {
	if r.Bus != nil {
		r.Bus.Halt()
	}
	r.workQueue.Stop()
	r.coordinator.Done()
	r.coordinator.Wait()
	r.state = RelayStopped

	log.Infof("Relay %s restarting.", r.Config.ID)
	r.coordinator.Add(1)
	r.state = RelayStarting
	r.workQueue.Start()
	r.startWorkers(r.worker)
	r.connectToCog()
	r.control <- RelayUpdateBundles
}

func (r *Relay) handleUpdateBundlesDone() {
	if r.state == RelayUpdatingBundles {
		if r.announce {
			r.announceBundles()
			r.announce = false
		}
		log.Info("Bundle refresh complete.")
		if r.hasStarted == false {
			log.Infof("Relay %s ready.", r.Config.ID)
			r.hasStarted = true
		}
		r.state = RelayReady
	} else {
		r.logBadState("handleUpdatesBundleDone", RelayUpdatingBundles)
	}
}

func (r *Relay) handleUpdateBundlesCommand() {
	if r.state == RelayStarting {
		log.Infof("Refreshing bundles and related assets every %s.", r.Config.RefreshDuration())
		r.setRefreshTimer()
		if r.Config.DockerEnabled() == true {
			log.Infof("Cleaning up expired Docker assets every %s.", r.Config.Docker.CleanDuration())
			r.setDockerTimer()
		}
	}
	msg := messages.ListBundlesEnvelope{
		ListBundles: &messages.ListBundlesMessage{
			RelayID: r.Config.ID,
			ReplyTo: r.Bus.DirectiveReplyTo(),
		},
	}
	raw, _ := json.Marshal(&msg)
	log.Info("Refreshing command bundles.")
	r.Bus.Publish("bot/relays/info", raw)
	r.state = RelayUpdatingBundles
}

func (r *Relay) logBadState(name string, required State) {
	log.Errorf("%s requires relay state %d: %d.", name, required, r.state)
}

func (r *Relay) announceBundles() {
	announcement := messages.NewBundleAnnouncement(r.Config.ID, r.BundleNames())
	raw, _ := json.Marshal(announcement)
	r.Bus.Publish(bus.RelayDiscoveryTopic, raw)
}

func (r *Relay) stopTimers() {
	if r.refreshTimer != nil {
		r.refreshTimer.Stop()
		r.refreshTimer = nil
	}
	if r.dockerTimer != nil {
		r.dockerTimer.Stop()
		r.dockerTimer = nil
	}
}

func (r *Relay) setRefreshTimer() {
	r.refreshTimer = time.AfterFunc(r.Config.RefreshDuration(), r.triggerRefresh)
}

func (r *Relay) setDockerTimer() {
	if r.Config.DockerEnabled() == false {
		return
	}
	r.dockerTimer = time.AfterFunc(r.Config.Docker.CleanDuration(), r.triggerDockerClean)
}

func (r *Relay) triggerRefresh() {
	r.UpdateBundles()
	r.setRefreshTimer()
}

func (r *Relay) triggerDockerClean() {
	if r.Config != nil {
		dockerEngine, err := engines.NewDockerEngine(*r.Config)
		if err != nil {
			panic(err)
		}
		count := dockerEngine.Clean()
		if count > 0 {
			log.Infof("Removed %d dead Docker containers.", count)
		}
	}
	r.setDockerTimer()
}

func computeBundleHash(bundles map[string]*config.Bundle) uint64 {
	keys := []string{}
	for k := range bundles {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := fnv.New64()
	for _, k := range keys {
		h.Sum([]byte(k))
	}
	return h.Sum64()
}
