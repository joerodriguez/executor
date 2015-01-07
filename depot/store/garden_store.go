package store

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"

	"github.com/cloudfoundry-incubator/executor"
	"github.com/cloudfoundry-incubator/executor/depot/log_streamer"
	"github.com/cloudfoundry-incubator/executor/depot/steps"
	"github.com/cloudfoundry-incubator/executor/depot/transformer"
	garden "github.com/cloudfoundry-incubator/garden/api"
	gardenclient "github.com/cloudfoundry-incubator/garden/client"
	"github.com/cloudfoundry/gunk/timeprovider"
	"github.com/pivotal-golang/lager"
	"github.com/tedsuo/ifrit"
)

type GardenStore struct {
	gardenClient       garden.Client
	exchanger          Exchanger
	containerOwnerName string

	healthyMonitoringInterval   time.Duration
	unhealthyMonitoringInterval time.Duration

	transformer  *transformer.Transformer
	timeProvider timeprovider.TimeProvider

	tracker InitializedTracker

	eventEmitter EventEmitter

	runningProcesses map[string]ifrit.Process
	processesL       sync.Mutex
}

type InitializedTracker interface {
	Initialize(executor.Container)
	Deinitialize(string)
	SyncInitialized([]executor.Container)
}

func NewGardenStore(
	gardenClient garden.Client,
	containerOwnerName string,
	containerMaxCPUShares uint64,
	containerInodeLimit uint64,
	healthyMonitoringInterval time.Duration,
	unhealthyMonitoringInterval time.Duration,
	transformer *transformer.Transformer,
	timeProvider timeprovider.TimeProvider,
	tracker InitializedTracker,
	eventEmitter EventEmitter,
) *GardenStore {
	return &GardenStore{
		gardenClient:       gardenClient,
		exchanger:          NewExchanger(containerOwnerName, containerMaxCPUShares, containerInodeLimit),
		containerOwnerName: containerOwnerName,

		healthyMonitoringInterval:   healthyMonitoringInterval,
		unhealthyMonitoringInterval: unhealthyMonitoringInterval,

		transformer:  transformer,
		timeProvider: timeProvider,

		tracker: tracker,

		eventEmitter: eventEmitter,

		runningProcesses: map[string]ifrit.Process{},
	}
}

func (store *GardenStore) Lookup(guid string) (executor.Container, error) {
	gardenContainer, err := store.gardenClient.Lookup(guid)
	if err != nil {
		return executor.Container{}, executor.ErrContainerNotFound
	}

	return store.exchanger.Garden2Executor(gardenContainer)
}

func (store *GardenStore) List(tags executor.Tags) ([]executor.Container, error) {
	filter := garden.Properties{
		ContainerOwnerProperty: store.containerOwnerName,
	}

	for k, v := range tags {
		filter[tagPropertyPrefix+k] = v
	}

	gardenContainers, err := store.gardenClient.Containers(filter)
	if err != nil {
		return nil, executor.ErrContainerNotFound
	}

	result := make([]executor.Container, 0, len(gardenContainers))

	for _, gardenContainer := range gardenContainers {
		container, err := store.exchanger.Garden2Executor(gardenContainer)
		if err != nil {
			continue
		}

		result = append(result, container)
	}

	return result, nil
}

func (store *GardenStore) Create(logger lager.Logger, container executor.Container) (executor.Container, error) {
	if container.State != executor.StateInitializing {
		return executor.Container{}, executor.ErrInvalidTransition
	}
	container.State = executor.StateCreated

	container, err := store.exchanger.CreateInGarden(logger, store.gardenClient, container)
	if err != nil {
		return executor.Container{}, err
	}

	store.tracker.Initialize(container)

	return container, nil
}

func (store *GardenStore) freeStepProcess(logger lager.Logger, guid string) (ifrit.Process, bool) {
	logger = logger.Session("freeing-step-process")
	logger.Info("started")
	defer logger.Info("finished")

	store.processesL.Lock()
	process, found := store.runningProcesses[guid]
	delete(store.runningProcesses, guid)
	store.processesL.Unlock()

	if !found {
		return nil, false
	}

	logger.Info("interrupting-process")
	process.Signal(os.Interrupt)
	return process, true
}

func (store *GardenStore) Stop(logger lager.Logger, guid string) error {
	logger = logger.Session("stopping-container", lager.Data{"container-guid": guid})
	logger.Info("started")
	defer logger.Info("finished")

	process, found := store.freeStepProcess(logger, guid)
	if !found {
		return executor.ErrContainerNotFound
	}

	<-process.Wait()
	return nil
}

func (store *GardenStore) Destroy(logger lager.Logger, guid string) error {
	logger = logger.Session("destroying-container", lager.Data{"container-guid": guid})
	logger.Info("started")

	store.freeStepProcess(logger, guid)

	err := store.gardenClient.Destroy(guid)
	if err != nil {
		logger.Error("failed-to-destroy-garden-container", err)
		return err
	}

	store.tracker.Deinitialize(guid)

	logger.Info("succeeded")
	return nil
}

func (store *GardenStore) GetFiles(guid, sourcePath string) (io.ReadCloser, error) {
	container, err := store.gardenClient.Lookup(guid)
	if err != nil {
		return nil, executor.ErrContainerNotFound
	}

	return container.StreamOut(sourcePath)
}

func (store *GardenStore) Ping() error {
	return store.gardenClient.Ping()
}

func (store *GardenStore) Run(logger lager.Logger, container executor.Container) error {
	logger = logger.Session("running-container", lager.Data{
		"container": container.Guid,
	})
	logger.Info("started")
	defer logger.Info("finished")

	gardenContainer, err := store.gardenClient.Lookup(container.Guid)
	if err == gardenclient.ErrContainerNotFound {
		logger.Error("garden-container-not-found", nil)
		return err
	} else if err != nil {
		logger.Error("failed-to-lookup-garden-container", err)
		return err
	}
	logger.Debug("found-garden-container")

	if container.State != executor.StateCreated {
		logger.Debug("container-invalid-state-transition", lager.Data{
			"current-state":  container.State,
			"required-state": executor.StateCreated,
		})

		transitionErr := executor.ErrInvalidTransition
		result := executor.ContainerRunResult{
			Failed:        true,
			FailureReason: transitionErr.Error(),
		}

		err := store.transitionToComplete(gardenContainer, result)
		if err != nil {
			logger.Error("failed-transition-to-complete", err)
		}

		return transitionErr
	}

	logStreamer := log_streamer.New(
		container.Log.Guid,
		container.Log.SourceName,
		container.Log.Index,
	)

	seq := []steps.Step{}

	if container.Setup != nil {
		seq = append(seq, store.transformer.StepFor(
			logStreamer,
			container.Setup,
			gardenContainer,
			container.ExternalIP,
			container.Ports,
			logger,
		))
	}

	parallelSequence := []steps.Step{
		store.transformer.StepFor(
			logStreamer,
			container.Action,
			gardenContainer,
			container.ExternalIP,
			container.Ports,
			logger,
		),
	}

	hasStartedRunning := make(chan struct{}, 1)

	var monitorStep steps.Step

	if container.Monitor != nil {
		monitoredStep := store.transformer.StepFor(
			logStreamer,
			container.Monitor,
			gardenContainer,
			container.ExternalIP,
			container.Ports,
			logger,
		)

		monitorStep = steps.NewMonitor(
			monitoredStep,
			hasStartedRunning,
			logger.Session("monitor"),
			store.timeProvider,
			time.Duration(container.StartTimeout)*time.Second,
			store.healthyMonitoringInterval,
			store.unhealthyMonitoringInterval,
		)
		parallelSequence = append(parallelSequence, monitorStep)
	} else {
		// this container isn't monitored, so we mark it running right away
		hasStartedRunning <- struct{}{}
	}

	seq = append(seq, steps.NewCodependent(parallelSequence))

	step := steps.NewSerial(seq)

	store.runStepProcess(logger, step, monitorStep, hasStartedRunning, gardenContainer, container.Guid)

	return nil
}

func (store *GardenStore) runStepProcess(
	logger lager.Logger,
	step steps.Step,
	monitorStep steps.Step,
	hasStartedRunning <-chan struct{},
	gardenContainer garden.Container,
	guid string,
) {
	process := ifrit.Invoke(ifrit.RunFunc(func(signals <-chan os.Signal, ready chan<- struct{}) error {
		logger := logger.Session("running-step-process")
		logger.Info("started")
		defer logger.Info("finished")
		seqComplete := make(chan error)

		close(ready)

		go func() {
			seqComplete <- step.Perform()
		}()

		result := executor.ContainerRunResult{}

	OUTER_LOOP:
		for {
			select {
			case <-signals:
				signals = nil
				logger.Debug("signaled")
				if monitorStep != nil {
					monitorStep.Cancel()
				}
				step.Cancel()

			case <-hasStartedRunning:
				hasStartedRunning = nil
				logger.Debug("transitioning-to-running")
				err := store.transitionToRunning(gardenContainer)
				if err != nil {
					logger.Error("failed-transitioning-to-running", err)
					result.Failed = true
					result.FailureReason = err.Error()
					break OUTER_LOOP
				}
				logger.Debug("succeeded-transitioning-to-running")

			case err := <-seqComplete:
				if err == nil {
					logger.Debug("step-finished-normally")
				} else {
					logger.Debug("step-finished-with-error", lager.Data{"error": err.Error()})
					result.Failed = true
					result.FailureReason = err.Error()
				}
				break OUTER_LOOP
			}
		}

		logger.Debug("transitioning-to-complete")
		err := store.transitionToComplete(gardenContainer, result)
		if err != nil {
			logger.Error("failed-transitioning-to-complete", err)
			return nil
		}
		logger.Debug("succeeded-transitioning-to-complete")

		return nil
	}))

	store.processesL.Lock()
	store.runningProcesses[guid] = process
	numProcesses := len(store.runningProcesses)
	store.processesL.Unlock()

	logger.Debug("stored-step-process", lager.Data{"num-step-processes": numProcesses})
}

func (store *GardenStore) transitionToRunning(gardenContainer garden.Container) error {
	err := gardenContainer.SetProperty(ContainerStateProperty, string(executor.StateRunning))
	if err != nil {
		return err
	}

	executorContainer, err := store.exchanger.Garden2Executor(gardenContainer)
	if err != nil {
		return err
	}

	store.eventEmitter.EmitEvent(executor.NewContainerRunningEvent(executorContainer))

	return nil
}

func (store *GardenStore) transitionToComplete(gardenContainer garden.Container, result executor.ContainerRunResult) error {
	resultJson, err := json.Marshal(result)
	if err != nil {
		return err
	}

	err = gardenContainer.SetProperty(ContainerResultProperty, string(resultJson))
	if err != nil {
		return err
	}

	err = gardenContainer.SetProperty(ContainerStateProperty, string(executor.StateCompleted))
	if err != nil {
		return err
	}

	executorContainer, err := store.exchanger.Garden2Executor(gardenContainer)
	if err != nil {
		return err
	}

	store.eventEmitter.EmitEvent(executor.NewContainerCompleteEvent(executorContainer))

	return nil
}

func (store *GardenStore) TrackContainers(interval time.Duration, logger lager.Logger) ifrit.Runner {
	logger = logger.Session("garden-store-track-containers")

	return ifrit.RunFunc(func(signals <-chan os.Signal, ready chan<- struct{}) error {
		ticker := store.timeProvider.NewTicker(interval)
		defer ticker.Stop()

		close(ready)

	dance:
		for {
			select {
			case <-signals:
				break dance

			case <-ticker.C():
				containers, err := store.List(nil)
				if err != nil {
					logger.Error("failed-to-list-containers", err)
					continue
				}

				store.tracker.SyncInitialized(containers)
			}
		}

		return nil
	})
}
