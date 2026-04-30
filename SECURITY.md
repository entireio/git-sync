# Security Policy

We take security seriously at Entire. We appreciate your efforts to responsibly disclose vulnerabilities and will make every effort to acknowledge your contributions.

## Reporting a Vulnerability

**Please do not report security vulnerabilities through public GitHub issues.**

Instead, please send security-related reports to **[security@entire.io](mailto:security@entire.io)**.

### What to Include

When reporting a vulnerability, please include:

1. **Description** - A clear description of the vulnerability
2. **Impact** - What an attacker could achieve by exploiting this issue
3. **Steps to reproduce** - Detailed steps to reproduce the vulnerability
4. **Affected versions** - Which versions of `git-sync` are affected (if known)
5. **Suggested fix** - If you have ideas on how to fix it (optional)

### What to Expect

- **Acknowledgment** - We will acknowledge receipt of your report within 48 hours
- **Updates** - We will keep you informed of our progress as we investigate
- **Resolution** - We aim to resolve critical vulnerabilities within 90 days

## Confidentiality

**All reports will be kept confidential.** We will not share your information with third parties without your consent, except as required by law.

## Supported Versions

We recommend always running the latest version of `git-sync`.

## Scope

This security policy applies to:

- The `git-sync` CLI and `git-sync-bench` benchmark command
- The `entire.io/entire/git-sync` and `entire.io/entire/git-sync/unstable` Go packages
- Official Entire GitHub repositories

### Out of Scope

The following are generally not considered security vulnerabilities:

- Issues in third-party dependencies (please report these upstream)
- Social engineering attacks
- Denial of service attacks against remotes you do not control
- Issues requiring physical access to a user's device

Because `git-sync` operates against Git remotes, please be especially careful when reporting issues that involve credentials, TLS verification, or remote-to-remote relay behavior — include the exact remote configuration that triggers the issue if it is reproducible.

---

## Security Advisories

Security advisories are issued when a confirmed vulnerability can be exploited by a remote or non-local actor. The following are generally treated as **bug reports rather than security advisories**:

- Regular expression performance issues (ReDoS) that only affect local execution
- Resource exhaustion that requires local access to trigger
- Issues that cannot be exploited without direct access to the user's machine or to credentials the user already controls

Use [GitHub Issues](https://github.com/entireio/git-sync/issues) to report bugs.

---

Thank you for helping keep `git-sync` and the Entire community safe!
