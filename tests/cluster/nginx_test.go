package cluster

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const nginxImage = "nginx:1.29-alpine"

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// selfSigned writes what the template expects to find in /etc/nginx/tls.
func selfSigned(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// The container runs as another user, and reads these before it drops its privileges.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, block := range map[string]*pem.Block{"fullchain.pem": {Type: "CERTIFICATE", Bytes: cert}, "privkey.pem": {Type: "EC PRIVATE KEY", Bytes: der}} {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(block), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The real nginx with the real template, in front of something that says what reached it. A browser can only put a token in
// the address of a WebSocket, so what nginx writes down decides whether it ends up in a log file.
func TestNginxKeepsTokensOutOfItsLogsAndNodeHeadersOutOfTheServer(t *testing.T) {
	if err := exec.Command("docker", "image", "inspect", nginxImage).Run(); err != nil {
		t.Skipf("needs docker and the %s image: %v", nginxImage, err)
	}
	template, err := filepath.Abs("../../infrastructure/nginx/streamkit.conf.template")
	if err != nil {
		t.Fatal(err)
	}
	tag := fmt.Sprintf("streamkit-nginx-test-%d", time.Now().UnixNano())
	network, upstream, edge := tag+"-net", tag+"-up", tag+"-edge"
	docker(t, "network", "create", network)
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", edge, upstream).Run()
		_ = exec.Command("docker", "network", "rm", network).Run()
	})

	// Everything the template proxies to, as one thing that repeats back the header in question.
	conf := `server { listen 7880; listen 3000; location / { default_type text/plain; return 200 "hop=[$http_x_streamkit_forwarded_by]"; } }`
	args := []string{"run", "-d", "--name", upstream, "--network", network, "--memory", "64m"}
	for _, alias := range []string{"server", "dashboard", "meeting", "grafana"} {
		args = append(args, "--network-alias", alias)
	}
	docker(t, append(args, nginxImage, "sh", "-c", "printf '%s' '"+conf+"' > /etc/nginx/conf.d/default.conf && nginx && exec sleep 600")...)
	docker(t, "run", "-d", "--name", edge, "--network", network, "--memory", "128m", "-e", "STREAMKIT_DOMAIN=rtc.test",
		// Where infrastructure/compose/production.yml puts it.
		"-v", template+":/etc/nginx/templates/default.conf.template:ro", "-v", selfSigned(t)+":/etc/nginx/tls:ro", "-p", "127.0.0.1::443", "-p", "127.0.0.1::80", nginxImage)
	logs := func() string {
		out, _ := exec.Command("docker", "logs", edge).CombinedOutput()
		return string(out)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("nginx said:\n%s", logs())
		}
	})
	httpsPort := docker(t, "port", edge, "443/tcp")
	httpPort := docker(t, "port", edge, "80/tcp")
	client := &http.Client{
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       10 * time.Second,
	}
	get := func(address string, headers map[string]string) (int, string, error) {
		req, err := http.NewRequest("GET", address, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "rtc.test"
		for k, v := range headers {
			if k == "Host" {
				req.Host = v
			} else {
				req.Header.Set(k, v)
			}
		}
		res, err := client.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer func() { _ = res.Body.Close() }()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body), nil
	}
	base := "https://" + httpsPort
	eventually(t, "nginx to answer", func() bool {
		status, _, err := get(base+"/rtc/validate", nil)
		return err == nil && status == 200
	})

	const secret = "THIS-IS-A-LIVE-TOKEN"
	want := func(address string, status int) string {
		t.Helper()
		got, body, err := get(address, map[string]string{"X-StreamKit-Forwarded-By": "any-node"})
		if err != nil {
			t.Fatal(err)
		}
		if got != status {
			t.Fatalf("%s: status %d, want %d: %s", address, got, status, body)
		}
		return body
	}

	// Through every door a token can come, with the server there.
	body := want(base+"/rtc/validate?access_token="+secret, 200)
	want(base+"/rtc?access_token="+secret, 200)
	want("http://"+httpPort+"/rtc?access_token="+secret, 301)
	// The image comes with a server of its own for this name, on port 80, which logs in the usual way.
	if status, _, err := get("http://"+httpPort+"/rtc?access_token="+secret, map[string]string{"Host": "localhost"}); err != nil || status != 301 {
		t.Fatalf("another name on port 80: %d %v", status, err)
	}
	// Reset and verify links are other one-time credentials in an address, and a page's next request repeats it.
	want(base+"/reset-password?token="+secret, 200)
	want(base+"/api/v1/rooms?token="+secret, 200)
	if status, _, err := get(base+"/_next/app.js", map[string]string{"Referer": base + "/reset-password?token=" + secret}); err != nil || status != 200 {
		t.Fatalf("a request with a referer: %d %v", status, err)
	}

	// Nothing a client sends for this reaches the server. It would be ignored there anyway.
	if body != "hop=[]" {
		t.Fatalf("the server was sent the header between nodes: %q", body)
	}

	// And with the server not answering, when nginx writes down the request it could not pass on.
	docker(t, "exec", upstream, "nginx", "-s", "stop")
	eventually(t, "the server to stop answering", func() bool {
		status, _, err := get(base+"/rtc/validate", nil)
		return err == nil && status == 502
	})
	want(base+"/rtc/validate?access_token="+secret, 502)
	want(base+"/rtc?access_token="+secret, 502)

	time.Sleep(200 * time.Millisecond)
	written := logs()
	if strings.Contains(written, secret) {
		t.Fatalf("a token is in nginx's logs:\n%s", written)
	}
	// It should still be useful for something: who asked for what, and how it went.
	if !strings.Contains(written, `"GET /rtc/validate HTTP/`) || !strings.Contains(written, ` 502 `) {
		t.Fatalf("the access log does not say what happened:\n%s", written)
	}
}
