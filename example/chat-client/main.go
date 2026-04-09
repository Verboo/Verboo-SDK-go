package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/Verboo/Verboo-SDK-go/pkg/frame"
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
	// Initialize logger
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
			o.TlsCfg = tlsCfg // Adding TLSConfig to Options
		},
		client.WithDownloadDir("./downloads"), // Set download directory for received files
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

	// Only body handler returns just the message body without any headers.
	c.AddOnMessage(func(msg *client.ParsedMessage) {
		line := fmt.Sprintf("From: %s", string(msg.Body))

		fmt.Fprintf(logView, "[green]%s\n", tview.Escape(line))
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

	// File received handler displays information about received files in TUI (from server Object Store)
	c.OnFileReceived(func(f *frame.ReceivedFile) {
		app.QueueUpdateDraw(func() {
			fmt.Fprintf(logView, "[green][FILE RECEIVED from Server] %s (%.1f MB) from %s\n",
				f.Filename, float64(f.Size)/1e6, f.SenderID)
			fmt.Fprintf(logView, "[cyan]Saved to: %s\n", f.LocalPath)
		})
	})

	// File available notification handler (new feature - server-mediated file transfer)
	c.OnFileAvailable(func(f *frame.FileAvailable) {
		app.QueueUpdateDraw(func() {})
	})

	// Create downloads directory at startup
	if err := os.MkdirAll("./downloads", 0755); err != nil {
		logger.S().Warnw("failed to create downloads directory", "err", err)
	}
	logView.Write([]byte(fmt.Sprintf("[yellow]Downloads directory: ./downloads\n")))

	// Input field handling for sending messages and file transfers
	input.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			text := strings.TrimSpace(input.GetText())
			if text == "" {
				return
			}

			// Handle file transfer command: /sendfile <path> (uses virtual streams - sliding window!)
			if strings.HasPrefix(text, "/sendfile ") {
				path := strings.TrimSpace(text[10:])
				fmt.Fprintf(logView, "[yellow]Uploading file via Virtual Stream: %s → target: %s (background mode)\n", path, *targetID)
				input.SetText("")
				go func() {
					err := c.SendVirtualStream(*targetID, path)
					app.QueueUpdateDraw(func() {
						if err != nil {
							fmt.Fprintf(logView, "[red]Failed to upload file: %v\n", err)
						} else {
							fmt.Fprintf(logView, "[green]File uploaded successfully via Virtual Stream\n")
						}
					})
				}()
				return
			}
			if strings.EqualFold(text, "/exit") || strings.EqualFold(text, "/quit") {
				app.Stop()
				return
			}

			// Handle download command: /download <file_id> (retrieves file from server Object Store)
			if strings.HasPrefix(text, "/download ") {
				fileID := strings.TrimSpace(text[10:])
				fmt.Fprintf(logView, "[yellow]Downloading file from Server Object Store: %s\n", fileID)

				// DownloadFile retrieves file from server Object Store with flow control (FrameFileAck)
				if err := c.DownloadFile(fileID); err != nil {
					fmt.Fprintf(logView, "[red]Failed to download file: %v\n", err)
				} else {
					fmt.Fprintf(logView, "[green]Download started from server Object Store...\n")
				}

				input.SetText("")
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
