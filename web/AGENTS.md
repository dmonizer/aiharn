# Web console

`app.js` is one IIFE. `index.html` has no inline script or style and uses a
strict CSP with `script-src 'self'`; all handlers belong in `app.js` via
`addEventListener`, with presentation in `styles.css`. Do not introduce inline
handlers, inline styles, or `eval`.

State is held in memory plus:

- `localStorage` for endpoint and active-session selection.
- `sessionStorage` for HTTP Basic credentials keyed by endpoint.

The client polls a complete `GET /api/v1/session` snapshot once per second and
keeps no transcript history of its own. It renders delivered agent messages from
history metadata and deliberately ignores `pending_agent_messages` to avoid
enqueue/delivery duplicates.

Composer behavior is text-sensitive: while a session is busy, an empty composer
shows Stop and posts `/api/v1/session/cancel`; any non-empty text shows Send and
posts `/api/v1/messages`, allowing mid-run follow-ups to queue. Preserve this
ordering in both button state and submit handling. A message accepted after the
server's stop boundary must not be removed by that cancellation.
