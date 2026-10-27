// Command runnerprobe sends lines to the XCUITest runner on an iPhone over
// usbmux and prints each reply with its round trip, which checks the runner
// without a server:
//
//	go run ./cmd/runnerprobe <udid> size 'tap 600 1300'
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/bmcreations/loupe/internal/usbmux"
)

func main() {
	port := flag.Int("port", 7110, "the runner's port on the phone")
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: runnerprobe [-port n] <udid> [command...]")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	c, err := usbmux.Connect(ctx, flag.Arg(0), *port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer c.Close()
	fmt.Printf("connected in %s\n", time.Since(start).Round(time.Millisecond))
	r := bufio.NewReader(c)
	for _, cmd := range flag.Args()[1:] {
		start := time.Now()
		fmt.Fprintf(c, "%s\n", cmd)
		reply, err := r.ReadString('\n')
		if err != nil {
			fmt.Fprintln(os.Stderr, cmd+":", err)
			os.Exit(1)
		}
		fmt.Printf("%-30s %-20q %s\n", cmd, reply[:len(reply)-1], time.Since(start).Round(time.Millisecond))
	}
}
