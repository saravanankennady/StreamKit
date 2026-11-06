// Package metrics holds the server's Prometheus collectors.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

const ns = "streamkit"

// Registry is the collector set exposed on /metrics.
var Registry = prometheus.NewRegistry()

func counter(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: name, Help: help}, labels)
	Registry.MustRegister(c)
	return c
}

func gauge(name, help string) prometheus.Gauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{Namespace: ns, Name: name, Help: help})
	Registry.MustRegister(g)
	return g
}

// Collectors. Packet and byte counters are labelled by direction ("in" from publishers, "out" to
// subscribers) and media kind ("audio" or "video").
var (
	Packets = counter("rtp_packets_total", "RTP packets processed.", "direction", "kind")
	Bytes   = counter("rtp_bytes_total", "RTP payload bytes processed.", "direction", "kind")
	// PacketsLost counts sequence-number gaps seen on publisher streams.
	PacketsLost = counter("rtp_packets_lost_total", "RTP packets missing from publisher streams.", "kind")
	NACKs       = counter("rtcp_nack_total", "NACK packets received from subscribers.")
	PLIs        = counter("rtcp_pli_total", "Keyframe requests sent to publishers.")
	FIRs        = counter("rtcp_fir_total", "FIR packets received from subscribers.")

	Rooms        = gauge("rooms", "Rooms currently open.")
	Participants = gauge("participants", "Participants currently in a room.")
	TracksIn     = gauge("tracks_published", "Tracks currently published.")
	TracksOut    = gauge("tracks_subscribed", "Track subscriptions currently active.")
	PeerConns    = gauge("peer_connections", "WebRTC peer connections currently open.")

	SignalConnects = counter("signal_connects_total", "Signalling connection attempts by outcome.", "outcome")
	SessionsEnded  = counter("sessions_ended_total", "Participant sessions ended, by reason.", "reason")

	DataPacketsDropped = counter("data_packets_dropped_total", "Data packets dropped for going over a participant's rate limit, by kind.", "kind")

	// Round-trip time and jitter come from subscriber RTCP receiver reports.
	SubscriberRTT = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: ns, Name: "subscriber_rtt_seconds", Help: "Round-trip time to subscribers.",
		Buckets: []float64{.005, .01, .02, .04, .08, .16, .32, .64, 1.28},
	})
	SubscriberJitter = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: ns, Name: "subscriber_jitter_seconds", Help: "Interarrival jitter reported by subscribers.",
		Buckets: []float64{.001, .002, .005, .01, .02, .04, .08, .16, .32},
	})
	SubscriberLoss = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: ns, Name: "subscriber_loss_ratio", Help: "Fraction of packets lost, per receiver report.",
		Buckets: []float64{.001, .005, .01, .02, .05, .1, .2, .5},
	})
)

func init() {
	Registry.MustRegister(
		SubscriberRTT, SubscriberJitter, SubscriberLoss,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}
