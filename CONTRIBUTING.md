# Contributing

Thanks for wanting to help. Bug reports, macOS test runs and pull requests are all welcome.

## The most useful things right now

- **Run it on a Mac.** The macOS build passes CI but has never run on a real Mac. Open a [macOS test report](https://github.com/SnlperStripes/claude-account-switcher/issues/new?template=macos_report.yml), even if everything worked.
- **Report breakage after a Claude update.** The switcher depends on how Claude stores its files. When an update changes that, a bug report with the Claude version and a log excerpt is the fastest way to a fix.

## Build and test

Go 1.26 or newer.

```bash
go vet ./...
go test ./...
go build -ldflags "-H=windowsgui -s -w" -o claude-account-switcher.exe .   # Windows
CGO_ENABLED=1 go build -o claude-account-switcher .                        # macOS
go build -ldflags "-s -w" -o claude-account-switcher .                     # Linux, needs libleveldb-dev
```

Every pull request runs vet and tests on Windows, macOS and Linux.

## Where things are

| File | What it does |
| --- | --- |
| `main.go` | Start-up, flags, log |
| `tray.go` | The tray menu |
| `switcher.go` | Accounts, `state.json`, the switch itself, auto-switch |
| `profiles.go` | Saving and restoring a sign-in, backups |
| `sessions.go` | Copying the Claude Code chat list between accounts |
| `usage.go` | Profile and usage requests to `api.anthropic.com`, polling, backoff |
| `update.go` | The daily check for a new release on GitHub |
| `desktop.go` | Reading Claude's data folder: `config.json`, cookies, the active account |
| `crypto_*.go` | Decrypting Claude's token cache (DPAPI, keychain, Secret Service) |
| `platform_*.go` | Finding, quitting and starting Claude, start at login, OS specifics |
| `icon.go` | The usage ring in the tray |

## Ground rules

These keep the tool safe to run on someone's real sign-ins:

- **Tokens never leave the machine** except to `api.anthropic.com`. Never log them, never write them in plain text.
- **Touch only what you must.** The switcher changes three `config.json` entries and the cookie file. Everything else in Claude's data stays as it is.
- **Fail loudly, restore the backup.** If Claude's files do not look as expected, stop, put the backup back and show the error in the menu. Do not guess.
- **No new dependencies** without talking about it in an issue first.

## Pull requests

- One change per pull request. Small is easier to review.
- Say what you tested on: Windows Store app, Windows direct download, macOS, Linux.
- Commit messages in plain English, imperative, like the existing history ("Poll usage less often and back off after 429").
- By contributing you agree that your work is licensed under [Apache 2.0](LICENSE), like the rest of the project.
