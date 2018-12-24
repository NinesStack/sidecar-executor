// UDPHook sends logs via brain-dead syslog loggers a la Sumo Logic's
// collector. Does not respect Syslog protocol at all. It simply fires loglines
// at a remote UDP address. If you want real Syslog, use the built-in Logrus
// SyslogHook.
package loghooks

import (
	"fmt"
	"net"
	"os"

	"github.com/sirupsen/logrus"
)

type UDPHook struct {
	Conn       net.Conn
	RemoteAddr string
}

func NewUDPHook(raddr string) (*UDPHook, error) {
	conn, err := net.Dial("udp", raddr)
	return &UDPHook{conn, raddr}, err
}

func (hook *UDPHook) Fire(entry *logrus.Entry) error {
	line, err := entry.String()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to read entry, %v", err)
		return err
	}

	hook.Conn.Write([]byte(line))

	return nil
}

func (hook *UDPHook) Levels() []logrus.Level {
	return logrus.AllLevels
}
