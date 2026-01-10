# Verboo-RTC SDK

The Verboo-RTC SDK provides a simple, high-performance client interface for connecting to the VerbooRTC server. It abstracts all transport details (WebSocket, HTTP/2, HTTP/3, QUIC) and handles authentication, connection management, and frame processing.

## Features

- **Transport Abstraction**: Supports WebSocket, HTTP/2 (gRPC), HTTP/3, and QUIC transports.
- **Automatic Reconnection**: Built-in exponential backoff with configurable min/max intervals.
- **Pooled Buffers**: Uses zero-copy frame handling for optimal performance (`EncodePooled`/`ReleaseEncoded`).
- **Presence Management**: Track online/offline status via `SetPresence()`.
- **Room Support**: Join/leave rooms and send messages to room members.
- **Delivery Guarantees**: Handle message delivery receipts (for future implementation).

## Installation

```bash
go get github.com/Verboo/Verboo-SDK-go/sdk
