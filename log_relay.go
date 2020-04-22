package main

import (
	"bufio"
	"io"
	"strings"
	"time"

	"github.com/Nitro/sidecar-executor/container"
	"github.com/Nitro/sidecar-executor/loghooks"
	log "github.com/sirupsen/logrus"
)

func (exec *sidecarExecutor) configureLogRelay(containerId string,
	labels map[string]string, output io.Writer) *log.Entry {

	syslogger := log.New()
	// We relay UDP syslog because we don't plan to ship it off the box
	// and because it's simplest since there is no backpressure issue to
	// deal with.
	hook, err := loghooks.NewUDPHook(exec.config.SyslogAddr)
	if err != nil {
		log.Fatalf("Error adding hook: %s", err)
	}

	syslogger.Hooks.Add(hook)
	syslogger.SetFormatter(&log.JSONFormatter{
		FieldMap: log.FieldMap{
			log.FieldKeyTime:  "Timestamp",
			log.FieldKeyLevel: "Level",
			log.FieldKeyMsg:   "Payload",
			log.FieldKeyFunc:  "Func",
		},
	})
	syslogger.SetOutput(output)

	// Add one to the labels length to account for hostname
	fields := make(log.Fields, len(exec.config.SendDockerLabels)+1)

	// Loop through the fields we're supposed to pass, and add them from the
	// Docker labels on this container
	for _, field := range exec.config.SendDockerLabels {
		if val, ok := labels[field]; ok {
			fields[field] = val
		}
	}
	fields["Hostname"] = exec.config.LogHostname

	return syslogger.WithFields(fields)
}

// relayLogs will watch a container and send the logs to Syslog
func (exec *sidecarExecutor) relayLogs(quitChan chan struct{},
	containerId string, labels map[string]string, output io.Writer) {

	logger := exec.configureLogRelay(containerId, labels, output)

	logger.Infof("sidecar-executor starting log pump for '%s'", containerId[:12])
	log.Info("Started syslog log pump") // Send to local log output

	outrd, outwr := io.Pipe()
	errrd, errwr := io.Pipe()

	// Tell Docker client to start pumping logs into our pipes
	container.FollowLogs(exec.client, containerId, 0, outwr, errwr)

	go exec.handleOneStream(quitChan, "stdout", logger, outrd)
	go exec.handleOneStream(quitChan, "stderr", logger, errrd)

	if exec.config.RelaySyslogStartupOnly {
		go cancelAfterStartup(quitChan, exec.config.RelaySyslogStartupTime)
	}

	<-quitChan
}

// cancelAfterStartup will stop the log pump after RelaySyslogStartupTime. This
// is used for apps that do their lown logging when started, but might fail
// during startup and need us to pump startup logs.
func cancelAfterStartup(quitChan chan struct{}, startupTime time.Duration) {
	<-time.After(startupTime)
	close(quitChan)
}

// handleOneStream will process one data stream into logs
func (exec *sidecarExecutor) handleOneStream(quitChan chan struct{}, name string,
	logger *log.Entry, in io.Reader) {

	scanner := bufio.NewScanner(in) // Defaults to splitting as lines

	for scanner.Scan() {
		// Before processing anything, see if we should be exiting.  Note that
		// this still doesn't exit until the _next_ log is processed after the
		// channel was closed.
		select {
		case <-quitChan:
			return
		default:
			// nothing
		}

		text := scanner.Text()
		log.Debugf("docker: %s", text)

		switch name {
		case "stdout":
			logger.Info(text) // Send to syslog "info"
		case "stderr":
			// Pretty basic attempt to scrape only errors from the logs
			if strings.Contains(strings.ToLower(text), "error") {
				logger.Error(text) // Send to syslog "error"
			} else {
				logger.Info(text) // Send to syslog "info"
			}
		default:
			log.Errorf("handleOneStream(): Unknown stream type '%s'. Exiting log pump.", name)
			return
		}
	}
	if err := scanner.Err(); err != nil {
		log.Errorf("handleOneStream() error reading Docker log input: '%s'. Exiting log pump '%s'.", err, name)
	}

	log.Warnf("Log pump exited for '%s'", name)
}
