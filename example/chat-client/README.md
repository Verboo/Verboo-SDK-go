# Two-way messaging between clients using Verboo-RTC SDK

## Step 1: Start the server
```bash
./verboo-rtc
```

## Step 2: Run two client instances in separate terminals
Terminal 1 (alice) transport websocket:
```bash
./verboo-sdk-chat-client -mode ws -user alice -target bob
```
Terminal 2 (bob) transport quic:
```bash
./verboo-sdk-chat-client -mode quic -addr localhost:8445 -user bob -target alice
```
## Step 3: Start chatting
In Terminal 1 (alice's):

```text
Hello from Alice!
```

In Terminal 2 (bob's), you'll see:
```text
[Full] From:alice To:bob Body:Hello from Alice!
[Filter:from] alice: Hello from Alice!
[Filter:ts] Time:1767950708067 Body:Hello from Alice!
[Only Body] Hello from Alice!
```

Now bob can reply in his terminal:

```text
Hi Alice! How are you?
```

And alice will see the response with all filtering options.

The SDK automatically handles message routing between users using their target IDs, demonstrating VerbooRTC's real-time messaging capabilities.
