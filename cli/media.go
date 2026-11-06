package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	streamkit "elnova-streamkit.local/sdks/server-go"
)

func egressCommand() *command {
	return &command{
		name:  "egress",
		usage: "record rooms and stream them elsewhere",
		subs: []*command{
			egressStartCommand(), egressListCommand(), egressGetCommand(), egressStopCommand(),
			egressDownloadCommand(), egressRetryCommand(), egressDeleteCommand(),
		},
	}
}

func egressStartCommand() *command {
	var kind, room, identity, layout, format string
	var streamURLs repeatedString
	return &command{
		name:  "start",
		usage: "start a recording or a stream output",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&kind, "type", "room-composite", "`TYPE` of job: room-composite, participant, audio-only or video-only")
			fs.StringVar(&room, "room", "", "`NAME` of the room")
			fs.StringVar(&room, "r", "", "`NAME` of the room")
			fs.StringVar(&identity, "identity", "", "`ID` of the participant, for one person's media")
			fs.StringVar(&identity, "i", "", "`ID` of the participant, for one person's media")
			fs.StringVar(&layout, "layout", "", "`LAYOUT` of a composite: grid or speaker")
			fs.StringVar(&format, "format", "", "record to a file in this `FORMAT`: mp4 or webm")
			fs.Var(&streamURLs, "stream-url", "stream to this rtmp:// or rtmps:// `URL`; can be repeated")
		},
		run: func(c *call) error {
			if room == "" {
				return usagef("--room is required")
			}
			if format == "" && len(streamURLs) == 0 {
				return usagef("say where the media goes: --format, --stream-url, or both")
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			o := streamkit.EgressOutput{Format: format, StreamURLs: streamURLs, Layout: layout}
			var job *streamkit.Egress
			switch strings.ReplaceAll(kind, "-", "_") {
			case streamkit.EgressRoomComposite:
				job, err = client.Egress.StartRoomCompositeEgress(c.ctx, room, o)
			case streamkit.EgressParticipant:
				if identity == "" {
					return usagef("--type participant needs --identity")
				}
				job, err = client.Egress.StartParticipantEgress(c.ctx, room, identity, o)
			case streamkit.EgressAudioOnly:
				job, err = client.Egress.StartAudioOnlyEgress(c.ctx, room, identity, o)
			case streamkit.EgressVideoOnly:
				job, err = client.Egress.StartVideoOnlyEgress(c.ctx, room, identity, o)
			default:
				return usagef("unknown --type %q. This server records room-composite, participant, audio-only and video-only", kind)
			}
			if err != nil {
				return err
			}
			return printJob(c, job)
		},
	}
}

func printJob(c *call, job *streamkit.Egress) error {
	return writeJSON(c.stdout, job)
}

func egressListCommand() *command {
	var room string
	var ids stringList
	var active bool
	var limit int
	return &command{
		name:    "list",
		aliases: []string{"ls"},
		usage:   "list jobs, newest first",
		flags: func(fs *flag.FlagSet) {
			fs.Var(&ids, "id", "only this job `ID`; can be repeated")
			fs.StringVar(&room, "room", "", "only this room `NAME`")
			fs.BoolVar(&active, "active", false, "only jobs that have not ended")
			fs.BoolVar(&active, "a", false, "only jobs that have not ended")
			fs.IntVar(&limit, "limit", 0, "show at most this many")
		},
		run: func(c *call) error {
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			keep := func(e *streamkit.Egress) bool {
				if room != "" && e.RoomName != room {
					return false
				}
				if active && e.Ended() {
					return false
				}
				if len(ids) == 0 {
					return true
				}
				for _, id := range ids {
					if id == e.ID {
						return true
					}
				}
				return false
			}
			// The API pages through everything, so these filters run here.
			var jobs []*streamkit.Egress
			cursor := ""
			for {
				page, err := client.Egress.ListEgress(c.ctx, streamkit.ListOptions{Cursor: cursor})
				if err != nil {
					return err
				}
				for _, e := range page.Items {
					if keep(e) && (limit == 0 || len(jobs) < limit) {
						jobs = append(jobs, e)
					}
				}
				cursor = page.NextCursor
				if cursor == "" || (limit > 0 && len(jobs) >= limit) {
					break
				}
			}
			if jobs == nil {
				jobs = []*streamkit.Egress{}
			}
			if c.global.json {
				return writeJSON(c.stdout, jobs)
			}
			tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tROOM\tTYPE\tSTATUS\tFORMAT\tBYTES\tERROR")
			for _, e := range jobs {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n", e.ID, e.RoomName, e.Type, e.Status, e.Format, e.FileSize, e.Error)
			}
			return tw.Flush()
		},
	}
}

func oneID(c *call, id string) (string, error) {
	if id == "" && len(c.args) == 1 {
		id = c.args[0]
	}
	if id == "" {
		return "", usagef("give an ID")
	}
	return id, nil
}

func egressGetCommand() *command {
	return &command{
		name:  "get",
		usage: "show one job",
		args:  "ID",
		run: func(c *call) error {
			id, err := oneID(c, "")
			if err != nil {
				return err
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			job, err := client.Egress.GetEgress(c.ctx, id)
			if err != nil {
				return err
			}
			return printJob(c, job)
		},
	}
}

func egressStopCommand() *command {
	var ids stringList
	var wait bool
	var timeout time.Duration
	return &command{
		name:  "stop",
		usage: "stop running jobs",
		args:  "[ID ...]",
		flags: func(fs *flag.FlagSet) {
			fs.Var(&ids, "id", "job `ID` to stop; can be repeated")
			fs.BoolVar(&wait, "wait", false, "wait until the job has really ended")
			fs.DurationVar(&timeout, "wait-timeout", 2*time.Minute, "how long --wait waits")
		},
		run: func(c *call) error {
			ids = append(ids, c.args...)
			if len(ids) == 0 {
				return usagef("give an ID")
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			for _, id := range ids {
				job, err := client.Egress.StopEgress(c.ctx, id)
				if err != nil {
					return fmt.Errorf("%s: %w", id, err)
				}
				if wait {
					ctx, cancel := context.WithTimeout(c.ctx, timeout)
					job, err = client.Egress.WaitForEgress(ctx, id, (*streamkit.Egress).Ended)
					cancel()
					if err != nil {
						return err
					}
				}
				if err := printJob(c, job); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func egressDownloadCommand() *command {
	var out string
	return &command{
		name:  "download",
		usage: "save a recording's file",
		args:  "ID",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&out, "out", "", "`FILE` to write; by default ID.format here")
			fs.StringVar(&out, "o", "", "`FILE` to write; by default ID.format here")
		},
		run: func(c *call) error {
			id, err := oneID(c, "")
			if err != nil {
				return err
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			if out == "" {
				job, err := client.Egress.GetRecording(c.ctx, id)
				if err != nil {
					return err
				}
				out = id + "." + job.Format
			}
			f, err := os.Create(out)
			if err != nil {
				return err
			}
			n, err := client.Egress.DownloadRecording(c.ctx, id, f)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				_ = os.Remove(out)
				return err
			}
			fmt.Fprintf(c.stderr, "Wrote %d bytes to %s\n", n, out)
			return nil
		},
	}
}

func egressRetryCommand() *command {
	return &command{
		name:  "retry",
		usage: "start a new job with the same request as a finished recording",
		args:  "ID",
		run: func(c *call) error {
			id, err := oneID(c, "")
			if err != nil {
				return err
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			job, err := client.Egress.RetryRecording(c.ctx, id)
			if err != nil {
				return err
			}
			return printJob(c, job)
		},
	}
}

func egressDeleteCommand() *command {
	return &command{
		name:  "delete",
		usage: "delete a finished recording and its file",
		args:  "ID",
		run: func(c *call) error {
			id, err := oneID(c, "")
			if err != nil {
				return err
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			return client.Egress.DeleteRecording(c.ctx, id)
		},
	}
}

func ingressCommand() *command {
	return &command{
		name:  "ingress",
		usage: "bring outside media into a room",
		subs: []*command{
			ingressCreateCommand(), ingressListCommand(), ingressGetCommand(), ingressUpdateCommand(), ingressDeleteCommand(),
			ingressStopCommand(), ingressRotateKeyCommand(), ingressLogsCommand(),
		},
	}
}

func ingressCreateCommand() *command {
	var input, room, identity, name, participantName, source string
	return &command{
		name:  "create",
		usage: "create an ingress. The response has the stream key, which is never shown again",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&input, "input", "", "`TYPE` of input: rtmp, whip or url")
			fs.StringVar(&room, "room", "", "`NAME` of the room")
			fs.StringVar(&room, "r", "", "`NAME` of the room")
			fs.StringVar(&identity, "identity", "", "`ID` the publisher appears as")
			fs.StringVar(&identity, "i", "", "`ID` the publisher appears as")
			fs.StringVar(&name, "name", "", "`NAME` of this ingress")
			fs.StringVar(&participantName, "participant-name", "", "display `NAME` the publisher appears as")
			fs.StringVar(&source, "source-url", "", "`URL` to pull from, for url input")
		},
		run: func(c *call) error {
			if input == "" || room == "" || identity == "" {
				return usagef("--input, --room and --identity are required")
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			out, err := client.Ingress.CreateIngress(c.ctx, &streamkit.CreateIngressRequest{
				InputType: input, Room: room, ParticipantIdentity: identity, Name: name, ParticipantName: participantName, SourceURL: source,
			})
			if err != nil {
				return err
			}
			return writeJSON(c.stdout, out)
		},
	}
}

func ingressListCommand() *command {
	var room string
	var ids stringList
	return &command{
		name:    "list",
		aliases: []string{"ls"},
		usage:   "list ingresses",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&room, "room", "", "only this room `NAME`")
			fs.Var(&ids, "id", "only this ingress `ID`; can be repeated")
		},
		run: func(c *call) error {
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			items, err := client.Ingress.ListIngress(c.ctx)
			if err != nil {
				return err
			}
			kept := []*streamkit.Ingress{}
			for _, in := range items {
				if room != "" && in.RoomName != room {
					continue
				}
				wanted := len(ids) == 0
				for _, id := range ids {
					wanted = wanted || id == in.ID
				}
				if wanted {
					kept = append(kept, in)
				}
			}
			if c.global.json {
				return writeJSON(c.stdout, kept)
			}
			tw := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tTYPE\tROOM\tIDENTITY\tSTATUS\tPUBLISH URL")
			for _, in := range kept {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", in.ID, in.Name, in.InputType, in.RoomName, in.ParticipantIdentity, in.Status, in.PublishURL)
			}
			return tw.Flush()
		},
	}
}

func ingressGetCommand() *command {
	return &command{
		name:  "get",
		usage: "show one ingress",
		args:  "ID",
		run: func(c *call) error {
			id, err := oneID(c, "")
			if err != nil {
				return err
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			out, err := client.Ingress.GetIngress(c.ctx, id)
			if err != nil {
				return err
			}
			return writeJSON(c.stdout, out)
		},
	}
}

func ingressUpdateCommand() *command {
	var req streamkit.UpdateIngressRequest
	optional := func(p **string) func(string) error {
		return func(v string) error {
			*p = &v
			return nil
		}
	}
	return &command{
		name:  "update",
		usage: "change an ingress. Changing the room or identity, or disabling it, ends a running stream",
		args:  "ID",
		flags: func(fs *flag.FlagSet) {
			fs.Func("name", "new `NAME`", optional(&req.Name))
			fs.Func("room", "new room `NAME`", optional(&req.Room))
			fs.Func("identity", "new `ID` the publisher appears as", optional(&req.ParticipantIdentity))
			fs.Func("participant-name", "new display `NAME` the publisher appears as", optional(&req.ParticipantName))
			fs.BoolFunc("enable", "enable it", func(string) error { req.Enabled = streamkit.Bool(true); return nil })
			fs.BoolFunc("disable", "disable it", func(string) error { req.Enabled = streamkit.Bool(false); return nil })
		},
		run: func(c *call) error {
			id, err := oneID(c, "")
			if err != nil {
				return err
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			out, err := client.Ingress.UpdateIngress(c.ctx, id, &req)
			if err != nil {
				return err
			}
			return writeJSON(c.stdout, out)
		},
	}
}

func ingressDeleteCommand() *command {
	var flagID string
	return &command{
		name:  "delete",
		usage: "delete an ingress and stop any running stream",
		args:  "ID",
		flags: func(fs *flag.FlagSet) { fs.StringVar(&flagID, "id", "", "`ID` of the ingress") },
		run: func(c *call) error {
			id, err := oneID(c, flagID)
			if err != nil {
				return err
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			if err := client.Ingress.DeleteIngress(c.ctx, id); err != nil {
				return err
			}
			fmt.Fprintf(c.stderr, "Deleted ingress %s\n", id)
			return nil
		},
	}
}

func ingressStopCommand() *command {
	return &command{
		name:  "stop",
		usage: "stop the running stream. The ingress can be used again",
		args:  "ID",
		run: func(c *call) error {
			id, err := oneID(c, "")
			if err != nil {
				return err
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			out, err := client.Ingress.StopIngress(c.ctx, id)
			if err != nil {
				return err
			}
			return writeJSON(c.stdout, out)
		},
	}
}

func ingressRotateKeyCommand() *command {
	return &command{
		name:  "rotate-key",
		usage: "replace the stream key. The old one stops working and its stream ends",
		args:  "ID",
		run: func(c *call) error {
			id, err := oneID(c, "")
			if err != nil {
				return err
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			out, err := client.Ingress.RotateIngressKey(c.ctx, id)
			if err != nil {
				return err
			}
			return writeJSON(c.stdout, out)
		},
	}
}

func ingressLogsCommand() *command {
	var limit int
	return &command{
		name:  "logs",
		usage: "show an ingress's events, newest first",
		args:  "ID",
		flags: func(fs *flag.FlagSet) { fs.IntVar(&limit, "limit", 0, "show at most this many") },
		run: func(c *call) error {
			id, err := oneID(c, "")
			if err != nil {
				return err
			}
			client, _, err := c.global.client()
			if err != nil {
				return err
			}
			items, err := client.Ingress.IngressLogs(c.ctx, id, limit)
			if err != nil {
				return err
			}
			return writeJSON(c.stdout, items)
		},
	}
}
