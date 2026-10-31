# Contributing to npm-switchyard

Thanks for your interest in npm-switchyard!

This project follows a [Code of Conduct](CODE_OF_CONDUCT.md); please be respectful.

> **Security issue?** Do **not** open a public issue; report it privately, see [SECURITY.md](SECURITY.md).

## Ways to contribute

- **Bug reports**: [open an issue](https://github.com/motwok/npm-switchyard/issues)
- **Feature requests**: [open an issue](https://github.com/motwok/npm-switchyard/issues)
- **Pull requests** are welcome, see below.

Ideas, reproductions and well-described problems are genuinely valuable too.

## Pull requests

npm-switchyard reads the Docker API, accepts connections from partner hosts and forwards network
traffic, so every change is reviewed carefully before it is merged. To make that easy:

- For anything larger than a small fix, open an issue first so the approach can be agreed on.
- Keep a pull request focused on one change. Work on a branch of your fork and target `master`.
- Use a Conventional Commit style title, for example `feat: ...`, `fix: ...`, `docs: ...`.
- Write code, comments and documentation in English. Use the Go standard library and the dependencies
  listed in [NOTICE](NOTICE); new dependencies need to be agreed in an issue first.
- Do not put raw non-ASCII characters into source code; write them as escape sequences (`\uXXXX`).
- Diagrams in Markdown are Mermaid, not ASCII art.
- The build, including the smoke test, must be green.

By submitting a pull request you agree that your contribution is licensed under the
**GNU AGPL v3** (see [LICENSE](LICENSE)).

## Writing a good bug report

Please include:

- **What happened** and **what you expected**.
- The **npm-switchyard version**: the image tag or digest.
- The **host platform** of both partners: Linux (`amd64`/`arm64`, kernel `uname -r`) or Windows
  (version and build), and the **Docker version**.
- Whether the problem is on the **importing** or the **exporting** side, and the name you used
  (`<container>.<port>.<partner>.switchyard` or `<container>.<partner>.switchyard`).
- The relevant lines of **`/metrics`** and of the **container log** (`docker logs <container>`) of both
  agents.
- **Steps to reproduce**, if you can; even rough ones help.

Remove host names, addresses or container names you do not want to make public before posting.

## Feature requests

Describe the problem you're trying to solve, not just a proposed solution; it helps find the best fit.
Note that not every request will be implemented; npm-switchyard intentionally stays small and focused.

## Forking

npm-switchyard is licensed under the **GNU AGPL v3** (see [LICENSE](LICENSE)). You are of course free to
fork and modify it under those terms.

## Support Me

If npm-switchyard helps you, you can support it: [Buy Me a Coffee](https://ko-fi.com/motwok)
