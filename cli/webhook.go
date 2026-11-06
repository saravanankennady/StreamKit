package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"elnova-streamkit.local/pkg/pb"
	streamkit "elnova-streamkit.local/sdks/server-go"
)

func webhookCommand() *command {
	return &command{
		name:  "webhook",
		usage: "test webhook receivers, and receive webhooks",
		subs:  []*command{webhookTestCommand(), webhookListenCommand()},
	}
}

// knownEvents are the events a server sends.
var knownEvents = map[string]bool{
	"room_started": true, "room_finished": true, "participant_joined": true, "participant_left": true,
	"participant_connection_aborted": true, "track_published": true, "track_unpublished": true,
	"egress_started": true, "egress_updated": true, "egress_ended": true, "ingress_started": true, "ingress_ended": true,
}

func webhookTestCommand() *command {
	var target, event, room, file string
	return &command{
		name:  "test",
		usage: "send a signed test event to a receiver, and report what it answered",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&target, "target", "", "`URL` of the receiver")
			fs.StringVar(&event, "event", "room_started", "`NAME` of the event")
			fs.StringVar(&room, "room", "streamkit-webhook-test", "room `NAME` in the event")
			fs.StringVar(&file, "body-file", "", "send this `FILE` as the body instead")
		},
		run: func(c *call) error {
			if target == "" {
				return usagef("--target is required")
			}
			if !knownEvents[event] {
				return usagef("unknown --event %q", event)
			}
			creds, err := c.global.credentials()
			if err != nil {
				return err
			}
			if err := creds.needSigning(); err != nil {
				return err
			}
			body, err := webhookBody(event, room, file)
			if err != nil {
				return err
			}
			signature, err := streamkit.SignWebhook(body, creds.APIKey, creds.secret)
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(c.ctx, http.MethodPost, target, bytes.NewReader(body))
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", signature)
			req.Header.Set("Content-Type", "application/webhook+json")
			req.Header.Set("User-Agent", "Elnova-StreamKit-CLI/"+version)
			client := &http.Client{Timeout: 10 * time.Second}
			res, err := client.Do(req)
			if err != nil {
				return fmt.Errorf("could not reach the receiver: %w", err)
			}
			defer res.Body.Close()
			answer, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
			if c.global.json {
				if err := writeJSON(c.stdout, map[string]any{"status": res.StatusCode, "event": event, "body": string(answer)}); err != nil {
					return err
				}
			} else {
				fmt.Fprintf(c.stdout, "Sent %s to %s: HTTP %d\n", event, target, res.StatusCode)
			}
			if res.StatusCode < 200 || res.StatusCode >= 300 {
				return fmt.Errorf("the receiver answered HTTP %d", res.StatusCode)
			}
			return nil
		},
	}
}

func webhookBody(event, room, file string) ([]byte, error) {
	if file != "" {
		return os.ReadFile(file)
	}
	return protojson.Marshal(&pb.WebhookEvent{
		Event: event, Id: fmt.Sprintf("EV_cli_test_%d", time.Now().UnixNano()), CreatedAt: time.Now().Unix(),
		Room: &pb.Room{Name: room, Sid: "RM_cli_test"},
	})
}

func webhookListenCommand() *command {
	var port, count int
	var bind string
	var timeout time.Duration
	return &command{
		name:  "listen",
		usage: "receive webhooks, check their signatures, and print the events",
		flags: func(fs *flag.FlagSet) {
			fs.IntVar(&port, "port", 9000, "`PORT` to listen on; 0 picks a free one, and prints it")
			fs.StringVar(&bind, "bind", "127.0.0.1", "`ADDRESS` to listen on")
			fs.IntVar(&count, "count", 0, "stop after this many good webhooks")
			fs.DurationVar(&timeout, "timeout", 0, "stop after this long")
		},
		run: func(c *call) error {
			creds, err := c.global.credentials()
			if err != nil {
				return err
			}
			if err := creds.needSigning(); err != nil {
				return err
			}
			ctx := c.ctx
			var cancel context.CancelFunc = func() {}
			if timeout > 0 {
				ctx, cancel = context.WithTimeout(ctx, timeout)
			}
			defer cancel()
			ctx, stop := context.WithCancel(ctx)
			defer stop()

			receiver := streamkit.NewWebhookReceiver(streamkit.Keys{creds.APIKey: creds.secret})
			received := 0
			events := make(chan *pb.WebhookEvent, 16)
			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				event, err := receiver.ReceiveRequest(r)
				if err != nil {
					fmt.Fprintf(c.stderr, "Rejected a webhook: %v\n", err)
					http.Error(w, "webhook could not be verified", http.StatusUnauthorized)
					return
				}
				events <- event
				w.WriteHeader(http.StatusOK)
			})
			listener, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(port)))
			if err != nil {
				return err
			}
			fmt.Fprintf(c.stderr, "Listening on http://%s\n", listener.Addr())
			server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
			go func() { _ = server.Serve(listener) }()
			defer server.Close()
			for {
				select {
				case <-ctx.Done():
					if errors.Is(ctx.Err(), context.DeadlineExceeded) && count > 0 && received < count {
						return fmt.Errorf("received %d of %d webhooks before the timeout", received, count)
					}
					return nil
				case event := <-events:
					received++
					data, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(event)
					if err != nil {
						return err
					}
					fmt.Fprintln(c.stdout, string(data))
					if count > 0 && received >= count {
						return nil
					}
				}
			}
		},
	}
}
