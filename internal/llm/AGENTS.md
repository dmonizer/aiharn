# LLM subsystem

`types.go` defines the provider-neutral contract: `Item`, `Request`,
`Client.Stream`, and the streaming event model from text deltas to terminal
completed or failed events. The rest of the application must not import provider
SDK types.

The `responses` and `chatcompletions` packages are wire-protocol adapters. Keep
translation and provider-specific behavior inside those packages. The agent owns
conversation history and rebuilds a complete stateless request for every turn.
