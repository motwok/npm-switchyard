# Changelog

The full, per-version changelog lives on the
**[Releases page](https://github.com/motwok/npm-switchyard/releases)**.

Every merge into `master` that changes the image is a release. Its notes are compiled **automatically**
from the merged pull requests by Release Drafter, grouped into **Features**, **Bug Fixes**,
**Documentation** and **Maintenance** by PR label; documentation and CI changes are listed with the next
release. Dependency updates that change the image are releases too. See [RELEASING.md](RELEASING.md).

Versioning follows [Semantic Versioning](https://semver.org). The next version is derived from PR labels:
`breaking`/`major` -> **major**, `feature`/`enhancement` -> **minor**, otherwise **patch**. Add the
`skip-changelog` label to omit a PR from the notes.

This file is an entry point and process description; it is intentionally not edited by hand per change.
