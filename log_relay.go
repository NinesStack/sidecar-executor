package main

import (
	"bufio"
	"io"
	"log/syslog"
	"os"

	"github.com/Nitro/sidecar-executor/container"
	"github.com/sirupsen/logrus"
	log "github.com/sirupsen/logrus"
	lSyslog "github.com/sirupsen/logrus/hooks/syslog"
)

func (exec *sidecarExecutor) configureLogRelay(containerId string, output io.Writer) *logrus.Entry {
	log := logrus.New()
	hook, err := lSyslog.NewSyslogHook("udp", "localhost:1514", syslog.LOG_INFO, "")

	if err != nil {
		log.Fatalf("Error adding hook: %s", err)
	}

	log.Hooks.Add(hook)
	log.SetFormatter(&logrus.JSONFormatter{
		FieldMap: logrus.FieldMap{
			logrus.FieldKeyTime:  "Timestamp",
			logrus.FieldKeyLevel: "Level",
			logrus.FieldKeyMsg:   "Payload",
			logrus.FieldKeyFunc:  "Func",
		},
	})
	log.SetOutput(output)

	return log.WithFields(logrus.Fields{
		"ServiceName": "foo-service",
		"Environment": "prod",
	})
}

// relayLogs will watch a container and send the logs to Syslog
func (exec *sidecarExecutor) relayLogs(quitChan chan struct{}, containerId string) {
	logger := exec.configureLogRelay(containerId, os.Stdout)

	logger.Info("sidecar-executor starting log pump for '%s'", containerId[:12])

	outrd, outwr := io.Pipe()
	errrd, errwr := io.Pipe()

	// Tell Docker client to start pumping logs into our pipes
	container.GetLogs(exec.client, containerId, 0, outwr, errwr)

	go exec.handleOneStream(quitChan, "stdout", logger, outrd)
	go exec.handleOneStream(quitChan, "stderr", logger, errrd)
}

// handleOneStream will process one data stream into logs
func (exec *sidecarExecutor) handleOneStream(quitChan chan struct{}, name string,
	logger *logrus.Entry, in io.Reader) {

	scanner := bufio.NewScanner(in) // Defaults to splitting as lines

	for scanner.Scan() {
		switch name {
		case "stdout":
			logger.Info(scanner.Text()) // Send to syslog "info"
		case "stderr":
			logger.Error(scanner.Text()) // Send to syslog "error"
		default:
			log.Error("handleOneStream(): Unknown stream type '%s'. Exiting log pump.", name)
			return
		}

		select {
		case <-quitChan:
			return
		default:
			// nothing
		}
	}
	if err := scanner.Err(); err != nil {
		log.Errorf("handleOneStream() error reading Docker log input: '%s'. Exiting log pump '%s'.", err, name)
	}
}
