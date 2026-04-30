# Contributing to git-sync

Thank you for your interest in contributing to Entire! We welcome contributions from everyone.

Please read our [Code of Conduct](CODE_OF_CONDUCT.md) before participating.

> **New to Entire?** See the [README](README.md) for setup and usage documentation.

---

## Before You Code: Discuss First

The fastest way to get a contribution merged is to align with maintainers before writing code. Please **open an issue first** using our [issue templates](https://github.com/entireio/git-sync/issues/new/choose) and wait for maintainer feedback before starting implementation.

### Contribution Workflow

1. **Open an issue** describing the problem or feature
2. **Wait for maintainer feedback** -- we may have relevant context or plans
3. **Get approval** before starting implementation
4. **Submit your PR** referencing the approved issue
5. **Address all feedback** including automated Copilot comments
6. **Maintainer review and merge**

---

## First-Time Contributors

New to the project? Welcome! Here's how to get started:

### Good First Issues

We recommend starting with:
- **Documentation improvements** - Fix typos, clarify explanations, add examples
- **Test contributions** - Add test cases, improve coverage
- **Small bug fixes** - Issues labeled `good-first-issue`

---

## Submitting Issues

All feature requests, bug reports, and general issues should be submitted through [GitHub Issues](https://github.com/entireio/git-sync/issues). Please search for existing issues before opening a new one.

For security-related issues, see the Security section below.

---

## Security

If you discover a security vulnerability, **do not report it through GitHub Issues**. Instead, please follow the instructions in our [SECURITY.md](SECURITY.md) file for responsible disclosure. All security reports are kept confidential as described in SECURITY.md.

---

## Contributions & Communication

Contributions and communications are expected to occur through:

- [GitHub Issues](https://github.com/entireio/git-sync/issues) - Bug reports and feature requests
- [Discord](https://discord.gg/jZJs3Tue4S) - Questions, general conversation, and real-time support

Please represent the project and community respectfully in all public and private interactions.


## How to Contribute


There are many ways to contribute:

- **Feature requests** - Open a [GitHub Issue](https://github.com/entireio/git-sync/issues) to discuss your idea
- **Bug reports** - Report issues via [GitHub Issues](https://github.com/entireio/git-sync/issues) (see [Reporting Bugs](#reporting-bugs))
- **Code contributions** - Fix bugs, add features, improve tests
- **Documentation** - Improve guides, fix typos, add examples
- **Community** - Help others, answer questions, share knowledge


## Reporting Bugs

Good bug reports help us fix issues quickly. When reporting a bug, please include:

### Required Information

1. **git-sync commit** - `git rev-parse HEAD` of the build you used (or release tag/version if applicable)
2. **Operating system**
3. **Go version** - run `go version`

### What to Include

Please answer these questions in your bug report:

1. **What did you do?** - Include the exact commands you ran
2. **What did you expect to happen?**
3. **What actually happened?** - Include the full error message or unexpected output
4. **Can you reproduce it?** - Does it happen every time or intermittently?
5. **Any additional context?** - Logs, screenshots, or related issues

---

## Local Setup

### Prerequisites

- **mise** - Task runner and version manager. Install with `curl https://mise.run | sh`

### Clone and Install

```bash
git clone https://github.com/entireio/git-sync.git
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

> See [docs/architecture.md](docs/architecture.md) for the architecture and package layout.

---

## Making Changes

1. **Create a branch** for your changes:
   ```bash
   git checkout -b feature/your-feature-name
   ```

2. **Make your changes** - follow the [Code Style](#code-style) guidelines

3. **Test your changes** - see [Testing](#testing)

4. **Commit** with clear, descriptive messages:
   ```bash
   git commit -m "Add feature: description of what you added"
   ```

---

## Code Style

Follow standard Go idioms and conventions.

### Key Points

- **Error handling**: Handle all errors explicitly - don't leave them unchecked
- **Formatting**: Code must pass `gofmt` (run `mise run fmt`)
- **Linting**: Code must pass `golangci-lint` (run `mise run lint`)
- **Naming**: Use meaningful, descriptive names following Go conventions
- **Public API**: `entire.io/entire/git-sync` is the stable embedding surface. Additions there should be reviewed carefully. `entire.io/entire/git-sync/unstable` is for advanced controls and may change.

---

## Testing

> See [docs/testing.md](docs/testing.md) for the full set of test suites and integration coverage.

```bash
# Default suite - always run before committing
mise run test

# With race detection
mise run test:ci

# Optional end-to-end against the system git-http-backend
mise run test:git-http-backend

# Optional live linux bootstrap smokes
mise run test:linux-smoke
mise run test:linux-smoke:batched
```

---

## Submitting a Pull Request

### Before You Submit

- **Related issue exists and is approved** -- Your PR references an issue where a maintainer has acknowledged the approach. (Exceptions: documentation fixes, typo corrections, and `good-first-issue` items.)
- **Linting passes** -- Run `mise run lint` (includes golangci-lint, gofmt, gomod, shellcheck)
- **Tests pass** -- Run `mise run test` to verify your changes
- **Tests included** -- New Go code and functionality should have accompanying tests
- **Entire checkpoint trailers included** -- See [Using Entire While Contributing](#using-entire-while-contributing) below

PRs that skip these steps are likely to be closed without merge.

### Submitting

1. **Push** your branch to your fork
2. **Open a PR** against the `main` branch
3. **Describe your changes** -- Link the related issue, summarize what changed and what testing you did
4. **Address Copilot feedback** -- See [Responding to Automated Review](#responding-to-automated-review)
5. **Wait for maintainer review**

---

## Responding to Automated Review

Copilot agent reviews every PR and provides feedback on code quality, potential bugs, and project conventions.

**Read and respond to every Copilot comment.** PRs with unaddressed Copilot feedback will not move to maintainer review.

- **Fixed** -- Push a commit addressing the issue.
- **Disagree** -- Reply explaining your reasoning. The Copilot isn't always right.
- **Question** -- Ask for clarification. We're happy to help.

Addressing Copilot feedback upfront is the fastest path to maintainer review.

---

## Using Entire While Contributing

We use Entire on Entire. When contributing, install the Entire CLI and let it capture your coding sessions -- this gives us valuable dogfooding data and helps improve the tool.

### Setup

Install the latest version of the Entire CLI (see [installation docs](https://docs.entire.io/cli/installation)) and verify with `entire version`. Entire is already configured in this repository, so there's no need to run `entire enable`.

### Checkpoint Trailers

All commits should include `Entire-Checkpoint` trailers from your sessions. These are added automatically by the `prepare-commit-msg` hook when Entire is enabled. The trailers link your commits to session metadata on the `entire/checkpoints/v1` branch.

### Sessions Branch

When you push your PR branch, Entire can automatically push the `entire/checkpoints/v1` branch alongside it (if `push_sessions` is enabled in your settings). Include this in your PR so maintainers can review the session context behind your changes.

---

## Troubleshooting

### Common Setup Issues

**`go mod download` fails with timeout**
```bash
# Try using direct mode
GOPROXY=direct go mod download
```

**`mise install` fails**
```bash
# Ensure mise is properly installed
curl https://mise.run | sh

# Reload your shell
source ~/.zshrc  # or ~/.bashrc
```

**Binary not updating after rebuild**
```bash
# Check which binary is being used
which git-sync
type -a git-sync

# You may have multiple installations - update the correct path
```

---

## Community

Join the Entire community:

- **Discord** - [Join our server][discord] for discussions and support

[discord]: https://discord.gg/jZJs3Tue4S

---

## Additional Resources

- [README](README.md) - Setup and usage documentation
- [docs/architecture.md](docs/architecture.md) - Architecture and package layout
- [docs/testing.md](docs/testing.md) - Test suites and integration coverage
- [Code of Conduct](CODE_OF_CONDUCT.md) - Community guidelines
- [Security Policy](SECURITY.md) - Reporting security vulnerabilities

---

Thank you for contributing!
