package main

import (
	"context"
	"errors"
	"io"
	"net/url"
	"time"

	mesos "github.com/mesos/mesos-go/api/v1/lib"
	"github.com/mesos/mesos-go/api/v1/lib/backoff"
	"github.com/mesos/mesos-go/api/v1/lib/encoding"
	"github.com/mesos/mesos-go/api/v1/lib/encoding/codecs"
	"github.com/mesos/mesos-go/api/v1/lib/executor"
	"github.com/mesos/mesos-go/api/v1/lib/executor/calls"
	"github.com/mesos/mesos-go/api/v1/lib/executor/config"
	"github.com/mesos/mesos-go/api/v1/lib/executor/events"
	"github.com/mesos/mesos-go/api/v1/lib/httpcli"
	"github.com/mesos/mesos-go/api/v1/lib/httpcli/httpexec"
	"github.com/pborman/uuid"
	log "github.com/sirupsen/logrus"
)

const (
	agentAPIPath = "/api/v1/executor"
	httpTimeout  = 10 * time.Second
)

type executorDriver struct {
	cli            calls.Sender
	cfg            config.Config
	framework      mesos.FrameworkInfo
	executor       mesos.ExecutorInfo
	agent          mesos.AgentInfo
	unackedTasks   map[mesos.TaskID]mesos.TaskInfo
	unackedUpdates map[string]executor.Call_Update
	failedTasks    map[mesos.TaskID]mesos.TaskStatus // send updates for these as we can
	shouldQuit     bool

	scExec *sidecarExecutor
}

func maybeReconnect(cfg *config.Config) <-chan struct{} {
	if cfg.Checkpoint {
		return backoff.Notifier(1*time.Second, cfg.SubscriptionBackoffMax*3/4, nil)
	}
	return nil
}

// unacknowledgedTasks generates the value of the UnacknowledgedTasks field of a Subscribe call.
func (state *executorDriver) unacknowledgedTasks() (result []mesos.TaskInfo) {
	if n := len(state.unackedTasks); n > 0 {
		result = make([]mesos.TaskInfo, 0, n)
		for k := range state.unackedTasks {
			result = append(result, state.unackedTasks[k])
		}
	}
	return
}

// unacknowledgedUpdates generates the value of the UnacknowledgedUpdates field of a Subscribe call.
func (state *executorDriver) unacknowledgedUpdates() (result []executor.Call_Update) {
	if n := len(state.unackedUpdates); n > 0 {
		result = make([]executor.Call_Update, 0, n)
		for k := range state.unackedUpdates {
			result = append(result, state.unackedUpdates[k])
		}
	}
	return
}

func (state *executorDriver) eventLoop(decoder encoding.Decoder,
	h events.Handler) (err error) {

	log.Info("Listening for events from agent")
	ctx := context.TODO()

	for err == nil && !state.shouldQuit {
		// housekeeping
		state.sendFailedTasks()

		var e executor.Event
		if err = decoder.Decode(&e); err == nil {
			err = h.HandleEvent(ctx, &e)
		}
	}
	return err
}

func (state *executorDriver) buildEventHandler() events.Handler {
	return events.HandlerFuncs{
		executor.Event_SUBSCRIBED: func(_ context.Context, e *executor.Event) error {
			log.Info("Executor subscribed to events")
			state.framework = e.Subscribed.FrameworkInfo
			state.executor = e.Subscribed.ExecutorInfo
			state.agent = e.Subscribed.AgentInfo
			return nil
		},

		executor.Event_LAUNCH: func(_ context.Context, e *executor.Event) error {
			state.unackedTasks[e.Launch.Task.TaskID] = e.Launch.Task
			state.scExec.LaunchTask(&e.Launch.Task)
			return nil
		},

		executor.Event_KILL: func(_ context.Context, e *executor.Event) error {
			state.scExec.KillTask(&e.Kill.TaskID)
			return nil
		},

		executor.Event_ACKNOWLEDGED: func(_ context.Context, e *executor.Event) error {
			delete(state.unackedTasks, e.Acknowledged.TaskID)
			delete(state.unackedUpdates, string(e.Acknowledged.UUID))
			return nil
		},

		executor.Event_MESSAGE: func(_ context.Context, e *executor.Event) error {
			log.Debugf("MESSAGE: received %d bytes of message data", len(e.Message.Data))
			return nil
		},

		executor.Event_SHUTDOWN: func(_ context.Context, e *executor.Event) error {
			log.Info("Shutting down the executor")
			state.shouldQuit = true
			return nil
		},

		executor.Event_ERROR: func(_ context.Context, e *executor.Event) error {
			log.Error("ERROR received")
			return errors.New(
				"received abort from Mesos, will attempt to re-subscribe",
			)

		},
	}.Otherwise(func(_ context.Context, e *executor.Event) error {
		log.Error("unexpected event", e)
		return nil
	})
}

func (state *executorDriver) sendFailedTasks() {
	for taskID, status := range state.failedTasks {
		updateErr := state.SendStatusUpdate(status)
		if updateErr != nil {
			log.Warnf(
				"failed to send status update for task %s: %+v", taskID.Value, updateErr,
			)
		} else {
			delete(state.failedTasks, taskID)
		}
	}
}

// SendStatusUpdate takes a new Mesos status and relays it to the agent
func (state *executorDriver) SendStatusUpdate(status mesos.TaskStatus) error {
	upd := calls.Update(status)
	resp, err := state.cli.Send(context.TODO(), calls.NonStreaming(upd))
	if resp != nil {
		resp.Close()
	}
	if err != nil {
		log.Errorf("failed to send update: %+v", err)
		logDebugJSON(upd)
	} else {
		state.unackedUpdates[string(status.UUID)] = *upd.Update
	}
	return err
}

func (state *executorDriver) newStatus(id mesos.TaskID) mesos.TaskStatus {
	return mesos.TaskStatus{
		TaskID:     id,
		Source:     mesos.SOURCE_EXECUTOR.Enum(),
		ExecutorID: &state.executor.ExecutorID,
		UUID:       []byte(uuid.NewRandom()),
	}
}

// StopDriver flags the event loop to exit on the next time around
func (exec *sidecarExecutor) StopDriver() {
	exec.driver.shouldQuit = true
}

// RunDriver runs the main event loop for the executor
func (exec *sidecarExecutor) RunDriver(mesosConfig *config.Config) {
	agentApiUrl := url.URL{
		Scheme: "http",
		Host:   mesosConfig.AgentEndpoint,
		Path:   agentAPIPath,
	}

	http := httpcli.New(
		httpcli.Endpoint(agentApiUrl.String()),
		httpcli.Codec(codecs.ByMediaType[codecs.MediaTypeProtobuf]),
		httpcli.Do(httpcli.With(httpcli.Timeout(httpTimeout))),
	)

	callOptions := executor.CallOptions{
		calls.Framework(mesosConfig.FrameworkID),
		calls.Executor(mesosConfig.ExecutorID),
	}

	driver := &executorDriver{
		cli: calls.SenderWith(
			httpexec.NewSender(http.Send),
			callOptions...,
		),
		unackedTasks:   make(map[mesos.TaskID]mesos.TaskInfo),
		unackedUpdates: make(map[string]executor.Call_Update),
		failedTasks:    make(map[mesos.TaskID]mesos.TaskStatus),
		scExec:         exec,
	}

	exec.driver = driver

	subscriber := calls.SenderWith(
		httpexec.NewSender(http.Send, httpcli.Close(true)),
		callOptions...,
	)

	shouldReconnect := maybeReconnect(mesosConfig)
	disconnectTime := time.Now()
	handler := driver.buildEventHandler()

	for {
		// Function block to ensure reponse is closed
		func() {
			subscribe := calls.Subscribe(
				driver.unacknowledgedTasks(),
				driver.unacknowledgedUpdates(),
			)

			log.Info("Subscribing to agent for events")

			resp, err := subscriber.Send(context.TODO(), calls.NonStreaming(subscribe))
			if resp != nil {
				defer resp.Close()
			}

			if err != nil && err != io.EOF {
				log.Error(err.Error())
				return
			}

			// we're officially connected, start decoding events
			err = driver.eventLoop(resp, handler)
			disconnectTime = time.Now()

			if err != nil && err != io.EOF {
				log.Error(err.Error())
				return
			}

			log.Info("Disconnected")
		}()

		if driver.shouldQuit {
			log.Info("Shutting down gracefully because we were told to")
			return
		}

		if !mesosConfig.Checkpoint {
			log.Info("Exiting gracefully because framework checkpointing is NOT enabled")
			return
		}

		if time.Now().Sub(disconnectTime) > mesosConfig.RecoveryTimeout {
			log.Errorf(
				"Failed to re-establish subscription with agent within %v, aborting",
				mesosConfig.RecoveryTimeout,
			)
			return
		}

		log.Info("Waiting for reconnect timeout")

		<-shouldReconnect // wait for some amount of time before retrying subscription
	}
}
