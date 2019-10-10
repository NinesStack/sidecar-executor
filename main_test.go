package main

import (
	"os"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

func Test_SetProcessName(t *testing.T) {
	Convey("Setting the process name", t, func() {
		originalLen := len(os.Args[0])

		Convey("bounds the name length to existing length", func() {
			SetProcessName(strings.Repeat("x", originalLen+10))
			So(len(os.Args[0]), ShouldEqual, originalLen)
		})

		Convey("modifies ARGV[0] correctly and pads name", func() {
			SetProcessName("decameron")
			So(os.Args[0], ShouldResemble, "decameron"+strings.Repeat(" ", originalLen-len("decameron")))
		})
	})
}
