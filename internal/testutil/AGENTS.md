# Test doubles

The `llm`, `execution`, and `approval` subpackages provide fakes for their
production interfaces. Use these in agent, tools, and app unit tests instead of
real providers, machines, or interactive approval clients.

Keep fakes deterministic and concurrency-safe. Execution transport/session
tests that require protocol behavior should use `internal/execution/ssh/harness`
rather than these higher-level doubles.
