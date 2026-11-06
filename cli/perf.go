package main

import (
	"flag"
	"fmt"
	"time"

	"elnova-streamkit.local/tests/load/loadgen"
)

func perfCommand() *command {
	return &command{
		name:  "perf",
		usage: "measure a server",
		subs:  []*command{perfLoadTestCommand()},
	}
}

func perfLoadTestCommand() *command {
	var (
		o                                loadgen.Options
		videoFile, audioFile             string
		numPerSecond                     int
		noVideo, noAudio                 bool
		videoPublishers, audioPublishers int
		publishers, subscribers          int
	)
	return &command{
		name:  "load-test",
		usage: "connect real WebRTC clients to a server and report what it sustained. The clients cost about as much CPU as the server, so results at scale need separate machines",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&o.Room, "room", "", "`NAME` of the room; by default a new one")
			fs.DurationVar(&o.Duration, "duration", 30*time.Second, "how long to measure, once everyone has connected")
			fs.IntVar(&publishers, "publishers", 0, "`NUMBER` of clients that publish video and audio, and subscribe")
			fs.IntVar(&videoPublishers, "video-publishers", 0, "`NUMBER` of clients that publish video")
			fs.IntVar(&audioPublishers, "audio-publishers", 0, "`NUMBER` of clients that publish audio")
			fs.IntVar(&subscribers, "subscribers", 0, "`NUMBER` of clients that only subscribe")
			fs.IntVar(&numPerSecond, "num-per-second", 50, "`NUMBER` of clients to start every second")
			fs.StringVar(&videoFile, "video", "", "VP8 .ivf `FILE` to send instead of the built-in test pattern")
			fs.StringVar(&audioFile, "audio", "", "Opus .ogg `FILE` to send instead of the built-in tone")
			fs.BoolVar(&noVideo, "no-video", false, "send no video")
			fs.BoolVar(&noAudio, "no-audio", false, "send no audio")
			fs.StringVar(&o.MetricsToken, "metrics-token", "", "`TOKEN` for the server's /metrics, which supplies its CPU and bandwidth figures")
		},
		run: func(c *call) error {
			if videoPublishers > 0 || audioPublishers > 0 {
				if publishers > 0 {
					return usagef("use --publishers, or --video-publishers and --audio-publishers, not both")
				}
				publishers = max(videoPublishers, audioPublishers)
			}
			if publishers+subscribers == 0 {
				return usagef("say how many clients to run, such as --publishers 2 --subscribers 10")
			}
			if numPerSecond <= 0 {
				return usagef("--num-per-second must be above zero")
			}
			creds, err := c.global.credentials()
			if err != nil {
				return err
			}
			if err := creds.needServer(); err != nil {
				return err
			}
			o.URL, o.Key, o.Secret = creds.URL, creds.APIKey, creds.secret
			o.Publishers, o.Subscribers = publishers, subscribers
			o.VideoPublishers, o.AudioPublishers = videoPublishers, audioPublishers
			o.RampEvery = time.Second / time.Duration(numPerSecond)

			video, audio, err := testPattern()
			if err != nil {
				return err
			}
			if videoFile != "" {
				if video, err = loadgen.LoadIVF(videoFile); err != nil {
					return fmt.Errorf("--video: %w", err)
				}
			}
			if audioFile != "" {
				if audio, err = loadgen.LoadOgg(audioFile); err != nil {
					return fmt.Errorf("--audio: %w", err)
				}
			}
			if !noVideo {
				o.Video = video
			}
			if !noAudio {
				o.Audio = audio
			}

			fmt.Fprintf(c.stderr, "Connecting %d publishers and %d subscribers. Results are only true of this machine and this network.\n", publishers, subscribers)
			r, err := loadgen.Run(c.ctx, o)
			if err != nil {
				return err
			}
			if err := writeJSON(c.stdout, r); err != nil {
				return err
			}
			if r.LastError != "" {
				fmt.Fprintln(c.stderr, "Last client error:", r.LastError)
			}
			if r.Failed > 0 || r.Tracks < r.ExpectedTracks {
				return fmt.Errorf("%d clients failed, and %d of %d tracks arrived", r.Failed, r.Tracks, r.ExpectedTracks)
			}
			return nil
		},
	}
}
