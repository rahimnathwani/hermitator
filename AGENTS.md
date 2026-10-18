# Working on hermitator

Read [docs/architecture.md](docs/architecture.md) first. The short version
of what must stay true:

- **Never start long-lived processes with `runsc exec`.** gVisor kills exec
  sessions on restore. Run them as spool jobs under the guest's PID 1
  (`internal/spool`). The web terminal is the one deliberate exception.
- **Treat everything under an agent's `shared/` and `spool/` as hostile.**
  The agent is root and can plant symlinks there. Host code touches those
  directories only through `os.Root` (`Spec.Shared()`, `spool.Client`).
- **A restore must see the same files the checkpoint saw.** Do not rewrite
  `config.json` or change the tools bundle of a checkpointed sandbox; only
  cold boots pick up new ones.
- **No credentials on command lines.** They travel in job stdin or a 0600
  file, and are scrubbed from the sandbox before suspend.
- **Agent state belongs to its actor** (`internal/orchestrator/actor.go`).
  Change it with `do` or `call`; never from another goroutine directly.

## Commands

```sh
make test     # go test -race ./...; includes end-to-end turns on the local runtime
make lint     # gofmt and go vet; CI also runs staticcheck
make dev      # panel on :8080 with HERMITATOR_RUNTIME=unsafe-local
make deploy   # build for linux and install on DEPLOY_HOST (see .env.example)
```

gVisor behaviour can only be checked on a Linux host. `hermitator doctor`
verifies one.

## Style

Go with the standard library first; a dependency needs a reason. In the
panel, anything that deletes, disconnects or discards asks first: give its
form `data-confirm` and a `<template data-confirm-body>` that says what
will happen to what, and how to undo it (`confirm_test.go` checks). Comments
explain why, not what. Commit messages say what changed and why, in plain
sentences, and tests that would have caught a bug ship with its fix.
