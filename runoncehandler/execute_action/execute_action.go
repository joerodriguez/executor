package execute_action

import (
	"github.com/cloudfoundry-incubator/executor/action_runner"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	steno "github.com/cloudfoundry/gosteno"
)

type ExecuteAction struct {
	runOnce      *models.RunOnce
	logger       *steno.Logger
	actionRunner *action_runner.ActionRunner
}

func New(
	runOnce *models.RunOnce,
	logger *steno.Logger,
	actionRunner *action_runner.ActionRunner,
) *ExecuteAction {
	return &ExecuteAction{
		runOnce:      runOnce,
		logger:       logger,
		actionRunner: actionRunner,
	}
}

func (action ExecuteAction) Perform(result chan<- error) {
	actionResult := make(chan error, 1)

	go action.actionRunner.Perform(actionResult)

	err := <-actionResult

	if err != nil {
		action.logger.Errord(
			map[string]interface{}{
				"runonce-guid": action.runOnce.Guid,
				"handle":       action.runOnce.ContainerHandle,
				"error":        err.Error(),
			},
			"runonce.actions.failed",
		)

		action.runOnce.Failed = true
		action.runOnce.FailureReason = err.Error()
	}

	result <- nil
}

func (action ExecuteAction) Cancel() {}

func (action ExecuteAction) Cleanup() {}
