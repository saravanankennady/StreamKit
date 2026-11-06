package worker

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"elnova-streamkit.local/egress/template"
)

const (
	maxChunkBytes  = 32 << 20
	bridgeWriteTTL = 5 * time.Second
)

// pageConfig is what the recorder page needs. It goes over the bridge rather than the page's URL, so the access token
// is never on a command line or in a log.
type pageConfig struct {
	URL          string   `json:"url"`
	Token        string   `json:"token"`
	Type         string   `json:"type"`
	Participant  string   `json:"participant,omitempty"`
	Layout       string   `json:"layout"`
	Width        int      `json:"width"`
	Height       int      `json:"height"`
	FPS          int      `json:"fps"`
	VideoBitrate int      `json:"videoBitrate"`
	AudioBitrate int      `json:"audioBitrate"`
	KeyframeMs   int      `json:"keyframeMs"`
	TimesliceMs  int      `json:"timesliceMs"`
	MimeTypes    []string `json:"mimeTypes"`
	VideoGraceMs int      `json:"videoGraceMs"`
}

// pageMessage is anything the page says. Binary frames are recording chunks instead.
type pageMessage struct {
	Type     string `json:"type"`
	Message  string `json:"message"`
	Level    string `json:"level"`
	Mime     string `json:"mime"`
	Reason   string `json:"reason"`
	Recorded bool   `json:"recorded"`

	FramesDrawn int64   `json:"framesDrawn"`
	Bytes       int64   `json:"bytes"`
	VideoTracks int     `json:"videoTracks"`
	AudioTracks int     `json:"audioTracks"`
	AudioState  string  `json:"audioState"`
	AudioTime   float64 `json:"audioTime"`
	// AudioPeak is how loud the mix has been since the last report, where 1 is full scale.
	AudioPeak float64 `json:"audioPeak"`
	// AudioPlaying and AudioMuted count the audio elements that are playing, and the tracks the browser says have no data.
	AudioPlaying int `json:"audioPlaying"`
	AudioMuted   int `json:"audioMuted"`
	// AudioIn is what is arriving for each audio track.
	AudioIn json.RawMessage `json:"audioIn"`
}

// bridge serves the recorder page, and connects each page that opens to its job. It listens on loopback and each
// connection must present a one-time token, which never appears on a command line, so nothing else on the machine can read
// a recording or feed one in, short of being able to read this user's files.
type bridge struct {
	log      *slog.Logger
	listener net.Listener
	server   *http.Server
	upgrader websocket.Upgrader

	mu      sync.Mutex
	waiting map[string]chan *websocket.Conn
}

func newBridge(log *slog.Logger) (*bridge, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := &bridge{log: log, listener: l, waiting: map[string]chan *websocket.Conn{}}
	origin := "http://" + l.Addr().String()
	b.upgrader = websocket.Upgrader{
		CheckOrigin:     func(r *http.Request) bool { return r.Header.Get("Origin") == origin },
		ReadBufferSize:  64 << 10,
		WriteBufferSize: 16 << 10,
	}
	files := http.FS(template.Files)
	static := http.FileServer(files)
	mux := http.NewServeMux()
	mux.HandleFunc("/bridge", b.handleBridge)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := fs.Stat(template.Files, trimSlash(r.URL.Path)); err != nil && r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		// The page talks to the room's server and to this bridge, and loads nothing else.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'unsafe-inline'; connect-src http: https: ws: wss:; media-src blob: mediastream:")
		static.ServeHTTP(w, r)
	}))
	b.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := b.server.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("bridge stopped", "error", err)
		}
	}()
	return b, nil
}

func trimSlash(p string) string {
	for len(p) > 0 && p[0] == '/' {
		p = p[1:]
	}
	return p
}

func (b *bridge) close() { _ = b.server.Close() }

// pageURL is where Chrome ends up for a token. The token sits in the fragment, which is never sent in a request, and the
// page removes it from the address as soon as it has read it.
func (b *bridge) pageURL(token string) string {
	return "http://" + b.listener.Addr().String() + "/index.html#token=" + token
}

// expect registers a one-time token. The connection arrives on the returned channel.
func (b *bridge) expect(token string) <-chan *websocket.Conn {
	ch := make(chan *websocket.Conn, 1)
	b.mu.Lock()
	b.waiting[token] = ch
	b.mu.Unlock()
	return ch
}

func (b *bridge) forget(token string) {
	b.mu.Lock()
	delete(b.waiting, token)
	b.mu.Unlock()
}

func (b *bridge) take(token string) chan *websocket.Conn {
	b.mu.Lock()
	defer b.mu.Unlock()
	for known, ch := range b.waiting {
		if subtle.ConstantTimeCompare([]byte(known), []byte(token)) == 1 {
			delete(b.waiting, known)
			return ch
		}
	}
	return nil
}

func (b *bridge) handleBridge(w http.ResponseWriter, r *http.Request) {
	ch := b.take(r.URL.Query().Get("token"))
	if ch == nil {
		http.Error(w, "unknown token", http.StatusUnauthorized)
		return
	}
	conn, err := b.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(maxChunkBytes)
	ch <- conn
}

func writeJSON(conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(bridgeWriteTTL))
	return conn.WriteMessage(websocket.TextMessage, data)
}
