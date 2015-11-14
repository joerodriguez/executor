package gardenhealth_test

import (
	"errors"
	"os"
	"time"

	"github.com/cloudfoundry-incubator/executor/gardenhealth"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"

	fakeexecutor "github.com/cloudfoundry-incubator/executor/fakes"
	"github.com/cloudfoundry-incubator/executor/gardenhealth/fakegardenhealth"
	"github.com/pivotal-golang/clock"
	"github.com/pivotal-golang/lager"
	"github.com/pivotal-golang/lager/lagertest"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

type fakeTimer struct {
	TimeChan chan time.Time
}

func newFakeTimer() *fakeTimer {
	return &fakeTimer{
		TimeChan: make(chan time.Time),
	}
}
func (t *fakeTimer) C() <-chan time.Time {
	return t.TimeChan
}

func (*fakeTimer) Reset(time.Duration) bool {
	return true
}
func (*fakeTimer) Stop() bool {
	return true
}

var _ = Describe("Runner", func() {
	var runner *gardenhealth.Runner
	var process ifrit.Process
	var logger *lagertest.TestLogger
	var checker *fakegardenhealth.FakeChecker
	var executorClient *fakeexecutor.FakeClient
	var timerProvider *fakegardenhealth.FakeTimerProvider
	var checkTimer *fakeTimer
	var timeoutTimer *fakeTimer

	BeforeEach(func() {
		logger = lagertest.NewTestLogger("test")
		checker = &fakegardenhealth.FakeChecker{}
		executorClient = &fakeexecutor.FakeClient{}
		timerProvider = &fakegardenhealth.FakeTimerProvider{}
		checkTimer = newFakeTimer()
		timeoutTimer = newFakeTimer()

		timers := []clock.Timer{checkTimer, timeoutTimer}
		timerProvider.NewTimerStub = func(time.Duration) clock.Timer {
			timer := timers[0]
			timers = timers[1:]
			return timer
		}
	})

	JustBeforeEach(func() {
		runner = gardenhealth.NewRunner(time.Minute, time.Minute, logger, checker, executorClient, timerProvider)
		process = ifrit.Background(runner)
	})

	AfterEach(func() {
		ginkgomon.Interrupt(process)
	})

	Describe("Run", func() {
		Context("When garden is immediately unhealthy", func() {

			Context("because the health check fails", func() {
				var checkErr = gardenhealth.UnrecoverableError("nope")
				BeforeEach(func() {
					checker.HealthcheckReturns(checkErr)
				})
				It("fails without becoming ready", func() {
					Eventually(process.Wait()).Should(Receive(Equal(checkErr)))
					Consistently(process.Ready()).ShouldNot(BeClosed())
				})
			})

			Context("because the health check timed out", func() {
				var blockHealthcheck chan struct{}

				BeforeEach(func() {
					blockHealthcheck = make(chan struct{})
					checker.HealthcheckStub = func(lager.Logger) error {
						<-blockHealthcheck
						return nil
					}
				})

				AfterEach(func() {
					close(blockHealthcheck)
				})

				It("fails without becoming ready", func() {
					Eventually(timeoutTimer.TimeChan).Should(BeSent(time.Time{}))
					Eventually(process.Wait()).Should(Receive(Equal(gardenhealth.HealthcheckTimeoutError{})))
					Consistently(process.Ready()).ShouldNot(BeClosed())
				})
			})
		})

		Context("When garden is healthy", func() {
			It("Sets healthy to true only once", func() {
				Eventually(executorClient.SetHealthyCallCount).Should(Equal(1))
				Expect(executorClient.SetHealthyArgsForCall(0)).Should(Equal(true))
				Eventually(checkTimer.TimeChan).Should(BeSent(time.Time{}))
				Expect(executorClient.SetHealthyCallCount()).To(Equal(1))
			})

			It("Continues to check at the correct interval", func() {
				Eventually(checker.HealthcheckCallCount).Should(Equal(1))
				Eventually(checkTimer.TimeChan).Should(BeSent(time.Time{}))
				Eventually(checkTimer.TimeChan).Should(BeSent(time.Time{}))
				Eventually(checkTimer.TimeChan).Should(BeSent(time.Time{}))
				Eventually(checker.HealthcheckCallCount).Should(Equal(4))
			})
		})

		Context("When garden is intermittently healthy", func() {
			var checkErr = errors.New("nope")

			It("reports unhealthy if we timeout", func() {
				Eventually(executorClient.SetHealthyCallCount).Should(Equal(1))

				Eventually(timeoutTimer.TimeChan).Should(BeSent(time.Time{}))
				Eventually(executorClient.SetHealthyCallCount).Should(Equal(2))
				Expect(executorClient.SetHealthyArgsForCall(1)).Should(Equal(false))
			})

			It("Sets healthy to false after it fails, then to true after success", func() {
				Eventually(executorClient.SetHealthyCallCount).Should(Equal(1))
				Expect(executorClient.SetHealthyArgsForCall(0)).Should(Equal(true))

				checker.HealthcheckReturns(checkErr)
				Eventually(checkTimer.TimeChan).Should(BeSent(time.Time{}))
				Eventually(executorClient.SetHealthyCallCount).Should(Equal(2))
				Expect(executorClient.SetHealthyArgsForCall(1)).Should(Equal(false))

				checker.HealthcheckReturns(nil)
				Eventually(checkTimer.TimeChan).Should(BeSent(time.Time{}))
				Eventually(executorClient.SetHealthyCallCount).Should(Equal(3))
				Expect(executorClient.SetHealthyArgsForCall(2)).Should(Equal(true))
			})
		})

		Context("When garden has an unrecoverable error", func() {
			var checkErr gardenhealth.UnrecoverableError = "huh"

			It("exits with an error", func() {
				Eventually(executorClient.SetHealthyCallCount).Should(Equal(1))

				checker.HealthcheckReturns(checkErr)
				Eventually(checkTimer.TimeChan).Should(BeSent(time.Time{}))
				Eventually(process.Wait()).Should(Receive(Equal(checkErr)))
			})
		})

		Context("When the runner is signaled", func() {

			Context("during the initial health check", func() {
				var blockHealthcheck chan struct{}

				BeforeEach(func() {
					blockHealthcheck = make(chan struct{})
					checker.HealthcheckStub = func(lager.Logger) error {
						<-blockHealthcheck
						return nil
					}
				})

				JustBeforeEach(func() {
					process.Signal(os.Interrupt)
				})

				It("exits with no error", func() {
					Eventually(process.Wait()).Should(Receive(BeNil()))
				})
			})

			Context("After the initial health check", func() {
				It("exits imediately with no error", func() {
					Eventually(executorClient.SetHealthyCallCount).Should(Equal(1))

					process.Signal(os.Interrupt)
					Eventually(process.Wait()).Should(Receive(BeNil()))
				})
			})
		})
	})
})