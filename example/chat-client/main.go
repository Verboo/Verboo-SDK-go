package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/Verboo/Verboo-SDK-go/pkg/logger"
	"github.com/Verboo/Verboo-SDK-go/sdk/client"
)

// Usage examples:
// WebSocket:   ./verboo-sdk-chat-client -mode ws -user alice -target bob
// HTTP/2:      ./verboo-sdk-chat-client -mode h2 -user alice -target bob -addr localhost:8443
// HTTP/3:      ./verboo-sdk-chat-client -mode h3 -user alice -target bob -addr localhost:8444
// QUIC:        ./verboo-sdk-chat-client -mode quic -user alice -target bob -addr localhost:8445
// gRPC:        ./verboo-sdk-chat-client -mode grpc -user alice -target bob -addr localhost:9443

// Main function initializes the Verboo-RTC SDK client with a TUI interface for interactive messaging.
func main() {
	// Инициализируем логгер
	logger.Init(logger.S())

	var (
		addr     = flag.String("addr", "localhost:8443", "server address (host:port)")
		mode     = flag.String("mode", "ws", "transport mode: ws, quic, h2, h3, grpc")
		insecure = flag.Bool("insecure", true, "skip TLS verification")
		userID   = flag.String("user", "cli-user", "user id for connection (default: cli-user)")
		targetID = flag.String("target", "bob", "target user id or room name (for sending messages)")
		debug    = flag.Bool("debug", false, "enable debug logging")

		certFile = flag.String("cert", "tls/tls.crt", "path to TLS certificate file")
		keyFile  = flag.String("key", "tls/tls.key", "path to TLS key file")
	)
	flag.Parse()

	// Generate JWT token for authentication
	token, err := client.GenerateToken(*userID, "")
	if err != nil {
		logger.S().Fatalf("failed to generate token: %v", err)
	}

	// Prepare TLS credentials for client
	tlsCfg := &tls.Config{
		InsecureSkipVerify: *insecure,
	}
	if *certFile != "" && *keyFile != "" {
		cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
		if err != nil {
			logger.S().Fatalf("failed to load TLS cert: %v", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	// Create SDK client with configuration options
	c, err := client.NewClient(
		client.WithToken(token),
		client.WithServerAddr(*addr),
		client.WithTransportType(*mode),
		client.WithReconnect(1*time.Second, 30*time.Second), // Configure exponential backoff for connections (1s to 30s)
		func(o *client.Options) {
			o.Insecure = *insecure
			o.Debug = *debug
			o.TlsCfg = tlsCfg // Добавляем TLSConfig в Options
		},
	)
	if err != nil {
		logger.S().Fatalf("failed to create client: %v", err)
	}

	// Initialize TUI application with proper styling and layout
	app := tview.NewApplication()

	logView := tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetChangedFunc(func() { app.Draw() })

	input := tview.NewInputField().
		SetLabel(fmt.Sprintf("to %s: ", *targetID)).
		SetFieldWidth(0)

	layout := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(logView, 0, 1, false).
		AddItem(input, 1, 0, true)

	// DEMONSTRATION HANDLERS for message filtering

	// Full handler demonstrates the complete message structure with all header fields.
	c.AddOnMessage(func(msg *client.ParsedMessage) {
		line := fmt.Sprintf("[Full] From:%s To:%s Body:%s",
			msg.Header.SenderID, msg.Header.TargetID, string(msg.Body))

		fmt.Fprintf(logView, "[white]%s\n", tview.Escape(line))
	},
		client.WithFilter([]string{"message_id", "sequence", "ts", "from", "to", "persistent"}))

	// Filter: from handler shows only the 'from' field and message body.
	c.AddOnMessage(func(msg *client.ParsedMessage) {
		line := fmt.Sprintf("[Filter:from] %s: %s",
			msg.Header.SenderID, string(msg.Body))

		fmt.Fprintf(logView, "[cyan]%s\n", tview.Escape(line))
	},
		client.WithFilter([]string{"from"}))

	// Filter: ts handler shows timestamp and body as a string.
	c.AddOnMessage(func(msg *client.ParsedMessage) {
		line := fmt.Sprintf("[Filter:ts] Time:%d Body:%s",
			msg.Header.Timestamp, string(msg.Body))

		fmt.Fprintf(logView, "[green]%s\n", tview.Escape(line))
	},
		client.WithFilter([]string{"ts"}),
		client.WithBodyAsString())

	// Only body handler returns just the message body without any headers.
	c.AddOnMessage(func(msg *client.ParsedMessage) {
		line := fmt.Sprintf("[Only Body] %s", string(msg.Body))

		fmt.Fprintf(logView, "[yellow]%s\n", tview.Escape(line))
	},
		client.WithIgnoreHeader())

	// Connection handler displays a success message when connected to the server.
	c.OnConnect(func() {
		fmt.Fprintf(logView, "[green]Connected to %s as %s\n", *addr, *userID)
	})

	// Disconnection handler shows an error message if disconnected unexpectedly.
	c.OnDisconnect(func(err error) {
		fmt.Fprintf(logView, "[red]Disconnected: %v\n", err)
	})

	// Input field handling for sending messages
	input.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			text := strings.TrimSpace(input.GetText())
			if text == "" {
				return
			}

			msg := fmt.Sprintf("[%s] %s", *userID, text)

			if err := c.SendTextMessage(*targetID, msg); err != nil {
				fmt.Fprintf(logView, "[red]Failed to send: %v\n", err)
			} else {
				fmt.Fprintf(logView, "[yellow][you]: %s\n", tview.Escape(text))
			}

			input.SetText("")
		}
	})

	// Background connection attempt
	go func() {
		if err := c.Connect(); err != nil {
			fmt.Fprintf(logView, "[red]Connection failed: %v\n", err)
		}
	}()

	// Run the TUI application
	if err := app.SetRoot(layout, true).Run(); err != nil {
		panic(err)
	}
}
