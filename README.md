# api-register-go (fork)

This fork is focused on the Go-based registration flow and the local dashboard in `api-register-go`.

## Fork-specific changes
- Added `Temp Mail` mode for dashboard-driven registration.
- Added automatic fallback from `temp-mail.org` to `mail.tm`.
- Added dashboard controls for:
  - Temp Mail parallel on/off
  - Worker count
  - Next-account delay
- Bound the Temp Mail mailbox creation cooldown to the same delay setting used for switching to the next account.
- Added terminal fallback for manual mailbox input and manual OTP input.
- Tightened OTP extraction to only capture `ChatGPT`-related 6-digit codes.
- Improved Temp Mail polling to reduce unnecessary requests and avoid missing late-arriving codes.

## Run

1. Start `register.exe`, or run:

```bash
go run .
```

2. Open:

```text
http://localhost:8899
```

## Notes

- Temp Mail parallel mode can trigger provider rate limits. In practice, `2-5` workers is the safer range.
- Successful tokens are written to the `tokens/` directory.
- Local runtime/config artifacts such as `tokens/`, `*.exe`, and temporary debug files are intentionally not meant for Git tracking.

## Build

```bash
go build -o register.exe .
```
