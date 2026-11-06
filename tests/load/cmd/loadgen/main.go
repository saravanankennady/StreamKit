// Command loadgen connects real WebRTC clients to a server and reports what the server sustained. See the loadgen
// package for what the clients do.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"elnova-streamkit.local/tests/load/loadgen"
)

func main() {
	var o loadgen.Options
	var videoFile, audioFile string
	flag.StringVar(&o.URL, "url", "ws://127.0.0.1:7880", "server address")
	flag.StringVar(&o.Key, "key", "devkey", "API key")
	flag.StringVar(&o.Secret, "secret", "secret", "API secret")
	flag.StringVar(&o.Room, "room", "", "room name; default is generated")
	flag.IntVar(&o.Publishers, "publishers", 2, "clients that publish and subscribe")
	flag.IntVar(&o.Subscribers, "subscribers", 0, "clients that only subscribe")
	flag.DurationVar(&o.Duration, "duration", 30*time.Second, "how long to measure, after everyone has connected")
	flag.DurationVar(&o.RampEvery, "ramp", 20*time.Millisecond, "delay between connecting clients")
	flag.StringVar(&videoFile, "video", "tests/load/sample.ivf", "VP8 sample; empty for no video")
	flag.StringVar(&audioFile, "audio", "tests/load/sample.ogg", "Opus sample; empty for no audio")
	flag.StringVar(&o.MetricsToken, "metrics-token", "", "token for the server's /metrics")
	flag.Parse()

	var err error
	if videoFile != "" {
		if o.Video, err = loadgen.LoadIVF(videoFile); err != nil {
			log.Fatalf("video sample: %v (generate it with tests/load/make-samples.sh)", err)
		}
	}
	if audioFile != "" {
		if o.Audio, err = loadgen.LoadOgg(audioFile); err != nil {
			log.Fatalf("audio sample: %v (generate it with tests/load/make-samples.sh)", err)
		}
	}

	r, err := loadgen.Run(context.Background(), o)
	if err != nil {
		log.Fatal(err)
	}
	out, _ := json.Marshal(r)
	fmt.Println(string(out))
	if r.LastError != "" {
		fmt.Fprintln(os.Stderr, "last client error:", r.LastError)
	}
}
