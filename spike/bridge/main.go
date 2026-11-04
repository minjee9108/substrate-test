// sshbridge: serves a WebSocket on :80/ssh; each connection runs `sshd -i`
// with the socket as its stdin/stdout. GET / returns ok for readiness.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/coder/websocket"
)

const hostKeyPath = "/etc/ssh/ssh_host_ed25519_key"

var hostKeyMu sync.Mutex

// ensureHostKey creates this actor's SSH host key the first time it is needed.
// It must not exist in the image or in the golden snapshot: both are shared by
// every actor from the template. Generated here, after an actor was cloned,
// it lives in that actor's own filesystem and survives its suspend/resume.
func ensureHostKey() error {
	hostKeyMu.Lock()
	defer hostKeyMu.Unlock()
	if _, err := os.Stat(hostKeyPath); err == nil {
		return nil
	}
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", hostKeyPath).CombinedOutput(); err != nil {
		return fmt.Errorf("ssh-keygen: %v: %s", err, strings.TrimSpace(string(out)))
	}
	fp, _ := exec.Command("ssh-keygen", "-lf", hostKeyPath+".pub").Output()
	log.Printf("generated host key for this actor: %s", strings.TrimSpace(string(fp)))
	return nil
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("/ssh", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			log.Printf("accept: %v", err)
			return
		}
		if err := ensureHostKey(); err != nil {
			log.Printf("host key: %v", err)
			c.Close(websocket.StatusInternalError, "host key unavailable")
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		nc := websocket.NetConn(ctx, c, websocket.MessageBinary)
		defer nc.Close()
		cmd := exec.CommandContext(ctx, "/usr/sbin/sshd", "-i", "-e")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = nc, nc, os.Stderr
		log.Printf("ssh session start")
		err = cmd.Run()
		log.Printf("ssh session end: %v", err)
	})
	log.Fatal(http.ListenAndServe(":80", mux))
}
