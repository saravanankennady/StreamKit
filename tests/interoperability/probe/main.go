// Command probe is a Pion client for the transport tests, which run it against each server and read its JSON. It does
// what a browser cannot: it controls RTP timestamps and sender reports exactly, and logs what reaches a subscriber.
//
//	probe av -url ws://127.0.0.1:7880 -for 12s -video-delay 150ms
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: probe av|watch [options]")
		os.Exit(2)
	}
	var result any
	var err error
	switch os.Args[1] {
	case "av":
		result, err = runAV(os.Args[2:])
	case "watch":
		result, err = runWatch(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(1)
	}
}
