# Contributing

Thanks for helping. Bug reports, hardware findings and pull requests are all
welcome.

## Before you open a pull request

- Open an issue first for anything larger than a small fix, so we can agree on
  the approach.
- Controller: `cd controller && go test ./...` passes, `gofmt` clean.
- Firmware: `firmware/build.sh amped-esp32-plus` builds with no new warnings.
  Changes to upstream snapclient go in `firmware/patches/` as numbered patches.
- Say what you tested on real hardware, if anything.

## Contributor license agreement

Retty Software maintains Civitone and also runs a hosted service built on it.
So that the project can keep being offered under its open-source licenses
and under other terms (for example to customers who can't accept the AGPL),
contributions are accepted under a contributor license agreement: you keep
the copyright in your work, and grant Retty Software a perpetual, worldwide,
royalty-free license to use, modify, sublicense and relicense it, including
under terms other than the project's licenses. You confirm you have the right
to make the contribution.

The first time you open a pull request you'll be asked to agree to it. Pull
requests can't be merged until you have.

## Security

Don't open public issues for security problems; see `SECURITY.md`.
