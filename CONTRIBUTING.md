# Contributing to git-sync

Thank you for your interest in contributing! `git-sync` is a remote-to-remote Git mirroring tool and library; we welcome contributions from everyone.

Please read our [Code of Conduct](CODE_OF_CONDUCT.md) before participating.

> **New here?** See the [README](README.md) for setup and usage, and [docs/architecture.md](docs/architecture.md) for the technical overview.

---

## Before You Code: Discuss First

The fastest way to get a contribution merged is to align with maintainers before writing code. Please **open an issue first** on [GitHub Issues](https://github.com/entireio/gitsync/issues) and wait for maintainer feedback before starting implementation.

### Contribution Workflow

1. **Open an issue** describing the problem or feature
2. **Wait for maintainer feedback** -- we may have relevant context or plans
3. **Get approval** before starting implementation
4. **Submit your PR** referencing the approved issue
5. **Address all feedback** including automated review comments
6. **Maintainer review and merge**

---

## First-Time Contributors

New to the project? Welcome! Good places to start:

- **Documentation improvements** - Fix typos, clarify explanations, add examples
- **Test contributions** - Add test cases, improve coverage of edge protocol behaviors
- **Small bug fixes** - Issues labeled `good-first-issue`

---

## Submitting Issues

All feature requests, bug reports, and general issues should be submitted through [GitHub Issues](https://github.com/entireio/gitsync/issues). Please search for existing issues before opening a new one.

For security-related issues, see [SECURITY.md](SECURITY.md) instead.

---

## How to Contribute

There are many ways to contribute:

- **Feature requests** - Open a [GitHub Issue](https://github.com/entireio/gitsync/issues) to discuss your idea
- **Bug reports** - Report issues via [GitHub Issues](https://github.com/entireio/gitsync/issues) (see [Reporting Bugs](#reporting-bugs))
- **Code contributions** - Fix bugs, add features, improve tests
- **Documentation** - Improve guides, fix typos, add examples
- **Community** - Help others, answer questions, share knowledge


## Reporting Bugs

Good bug reports help us fix issues quickly. When reporting a bug, please include:

### Required Information

1. **`git-sync` version or commit** - the binary you ran or `git rev-parse HEAD` if building from source
2. **Operating system**
3. **Go version** - run `go version`
4. **Source and target hosts** - what kind of remote (GitHub, GitLab, self-hosted, etc.) — this matters because protocol behavior differs

### What to Include

1. **What did you do?** - The exact `git-sync` command you ran (redact tokens)
2. **What did you expect to happen?**
3. **What actually happened?** - Full error message, and `--json` output if available
4. **Can you reproduce it?** - Every time, or intermittently?
5. **Any additional context?** - `--stats` output, `-v` verbose log, related issues

---

## Local Setup

### Prerequisites

- **Go 1.26.x** - Check with `go version`
- **mise** - Task runner and version manager. Install with `curl https://mise.run | sh`

### Clone and Build

```bash
git clone https://github.com/entireio/gitsync.git
cd gitsync

# Trust the mise configuration (required on first setup)
mise trust

# Install dependencies (mise will install the correct Go version)
mise install

# Download Go modules
go mod download

# Build the CLI
mise run build

# Verify setup by running tests
mise run test
```

---

## Making Changes

1. **Create a branch** for your changes:
   ```bash
   git checkout -b your-name/feature-name
   ```

2. **Make your changes** - follow the [Code Style](#code-style) guidelines.

3. **Test your changes** - see [Testing](#testing).

4. **Commit** with clear, descriptive messages.

---

## Code Style

Follow standard Go idioms and conventions.

### Key Points

- **Error handling**: Handle all errors explicitly - don't leave them unchecked
- **Formatting**: Code must pass `gofmt` (run `mise run fmt`)
- **Linting**: Code must pass `golangci-lint` (run `mise run lint`)
- **Naming**: Use meaningful, descriptive names following Go conventions
- **Public API**: `entire.io/entire/gitsync` is the stable embedding surface. Additions there should be reviewed carefully. `entire.io/entire/gitsync/unstable` is for advanced controls and may change. See [docs/embedding.md](docs/embedding.md).

---

## Testing

```bash
# Default suite (in-process smart HTTP, no listener required)
mise run test

# With race detection
mise run test:ci

# Optional: end-to-end against the system git-http-backend
mise run test:git-http-backend

# Optional: live linux bootstrap smoke (downloads from github)
mise run test:linux-smoke
mise run test:linux-smoke:batched
```

See [docs/testing.md](docs/testing.md) for the full list of suites and environment flags.

---

## Submitting a Pull Request

### Before You Submit

- **Related issue exists and is approved** -- Your PR references an issue where a maintainer has acknowledged the approach. (Exceptions: documentation fixes, typo corrections, and `good-first-issue` items.)
- **Linting passes** -- Run `mise run lint`
- **Tests pass** -- Run `mise run test`
- **Tests included** -- New Go code and behavior changes should have accompanying tests. Protocol-level changes should ideally be covered both in `internal/gitproto` unit tests and in an `internal/syncer` integration test.

PRs that skip these steps are likely to be closed without merge.

### Submitting

1. **Push** your branch to your fork
2. **Open a PR** against the `main` branch
3. **Describe your changes** -- Link the related issue, summarize what changed and what testing you did
4. **Address automated review feedback**
5. **Wait for maintainer review**

---

## Community

- **GitHub Issues** - bug reports, feature discussions
- **Discord** - [Join our server](https://discord.gg/jZJs3Tue4S) for questions and real-time conversation

---

## Additional Resources

- [README](README.md) - Setup and usage documentation
- [docs/architecture.md](docs/architecture.md) - Technical architecture and package layout
- [docs/embedding.md](docs/embedding.md) - Library embedding guide
- [docs/testing.md](docs/testing.md) - Test suites and integration coverage
- [Code of Conduct](CODE_OF_CONDUCT.md) - Community guidelines
- [Security Policy](SECURITY.md) - Reporting security vulnerabilities

---

Thank you for contributing!
