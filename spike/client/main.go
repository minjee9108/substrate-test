// sshproxy: ssh ProxyCommand. Usage: sshproxy ws://127.0.0.1:8000/ssh
package main

import (
	"context"
	"io"
	"log"
	"os"

	"github.com/coder/websocket"
)

func main() {
	ctx := context.Background()
	c, _, err := websocket.Dial(ctx, os.Args[1], nil)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	nc := websocket.NetConn(ctx, c, websocket.MessageBinary)
	done := make(chan struct{}, 2)
	go func() { io.Copy(nc, os.Stdin); done <- struct{}{} }()
	go func() { io.Copy(os.Stdout, nc); done <- struct{}{} }()
	<-done
}
