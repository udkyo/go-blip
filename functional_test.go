package blip

import (
	"expvar"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	assert "github.com/couchbaselabs/go.assert"
	"golang.org/x/net/websocket"
)

// Round trip a high number of messages over a loopback websocket
// It was originally an attempt to repro SG issue https://github.com/couchbase/sync_gateway/issues/3221 isolated
// to go-blip.  This test was not able to repro that particular error, but is useful in it's own right as a
// regression test or starting point for other test cases that require two peers running over an actual websocket,
// aka a "functional test".
func TestEchoRoundTrip(t *testing.T) {

	blipContextEchoServer, err := NewContext(BlipTestAppProtocolId)
	if err != nil {
		t.Fatal(err)
	}

	receivedRequests := sync.WaitGroup{}

	// ----------------- Setup Echo Server  -------------------------

	// Create a blip profile handler to respond to echo requests and then abruptly close the socket
	dispatchEcho := func(request *Message) {
		defer receivedRequests.Done()
		body, err := request.Body()
		if err != nil {
			log.Printf("ERROR reading body of %s: %s", request, err)
			return
		}
		if request.Properties["Content-Type"] != "application/octet-stream" {
			t.Fatalf("Incorrect properties: %#x", request.Properties)
		}
		if response := request.Response(); response != nil {
			response.SetBody(body)
			response.Properties["Content-Type"] = request.Properties["Content-Type"]
		}

	}

	// Blip setup
	blipContextEchoServer.HandlerForProfile["BLIPTest/EchoData"] = dispatchEcho
	blipContextEchoServer.LogMessages = false
	blipContextEchoServer.LogFrames = false

	// Websocket Server
	server := blipContextEchoServer.WebSocketServer()
	defaultHandler := server.Handler
	server.Handler = func(conn *websocket.Conn) {
		defer func() {
			conn.Close() // in case it wasn't closed already
		}()
		defaultHandler(conn)
	}

	// HTTP Handler wrapping websocket server
	mux := http.NewServeMux()
	mux.Handle("/blip", defaultHandler)
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		t.Error(http.Serve(listener, mux))
	}()

	// ----------------- Setup Echo Client ----------------------------------------

	blipContextEchoClient, err := NewContext(BlipTestAppProtocolId)
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	destUrl := fmt.Sprintf("ws://localhost:%d/blip", port)
	sender, err := blipContextEchoClient.Dial(destUrl, "http://localhost")
	if err != nil {
		t.Fatalf("Error opening WebSocket: %v", err)
	}

	numRequests := 100
	receivedRequests.Add(numRequests)

	for i := 0; i < numRequests; i++ {

		// Create echo request
		echoRequest := NewRequest()
		echoRequest.SetProfile("BLIPTest/EchoData")
		echoRequest.Properties["Content-Type"] = "application/octet-stream"
		echoRequest.SetBody([]byte("hello"))

		// Send echo request
		sent := sender.Send(echoRequest)
		assert.True(t, sent)

		// Have multiple readers simultaneously trying to fetch multiple responses for each message and assert on the body.
		// reliably triggers races on m.Response and response.Body that have been seen only rarely in Sync Gateway testing.
		for i := 0; i < 5; i++ {
			go func() {
				for j := 0; j < 10; j++ {
					go func(m *Message) {
						response := m.Response()
						if response == nil {
							t.Errorf("unexpected nil message response")
							return
						}
						responseBody, err := response.Body()
						assert.True(t, err == nil)
						assert.Equals(t, string(responseBody), "hello")
					}(echoRequest)
				}
			}()
		}

	}

	// Wait until all requests were sent and responded to
	receivedRequests.Wait()

}

// TestSenderPing ensures a client configured with a WebsocketPingInterval sends ping frames on an otherwise idle connection.
func TestSenderPing(t *testing.T) {

	// server
	serverCtx, err := NewContext(BlipTestAppProtocolId)
	if err != nil {
		t.Fatal(err)
	}
	server := serverCtx.WebSocketServer()
	defaultHandler := server.Handler
	server.Handler = func(conn *websocket.Conn) {
		defer func() {
			conn.Close() // in case it wasn't closed already
		}()
		defaultHandler(conn)
	}
	mux := http.NewServeMux()
	mux.Handle("/blip", defaultHandler)
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		t.Error(http.Serve(listener, mux))
	}()

	// client
	clientCtx, err := NewContext(BlipTestAppProtocolId)
	if err != nil {
		t.Fatal(err)
	}
	clientCtx.LogMessages = true
	clientCtx.LogFrames = true
	clientCtx.WebsocketPingInterval = time.Millisecond * 10

	port := listener.Addr().(*net.TCPAddr).Port
	destUrl := fmt.Sprintf("ws://localhost:%d/blip", port)

	// client hasn't connected yet, stats are uninitialized
	assert.Equals(t, expvarToInt64(goblipExpvar.Get("sender_ping_count")), int64(0))
	assert.Equals(t, expvarToInt64(goblipExpvar.Get("goroutines_sender_ping")), int64(0))

	sender, err := clientCtx.Dial(destUrl, "http://localhost")
	if err != nil {
		t.Fatalf("Error opening WebSocket: %v", err)
	}

	time.Sleep(time.Millisecond * 50)

	// an active ping goroutine with at least 1 ping sent for the above sleep
	assert.Equals(t, expvarToInt64(goblipExpvar.Get("goroutines_sender_ping")), int64(1))
	assert.True(t, expvarToInt64(goblipExpvar.Get("sender_ping_count")) > 0)

	sender.Close()

	// ensure the sender's ping goroutine has exited
	assert.Equals(t, expvarToInt64(goblipExpvar.Get("goroutines_sender_ping")), int64(0))
}

func expvarToInt64(v expvar.Var) int64 {
	if v == nil {
		return 0
	}
	i, _ := strconv.ParseInt(v.String(), 10, 64)
	return i
}
