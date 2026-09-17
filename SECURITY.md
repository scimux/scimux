# Security policy

## Reporting a vulnerability

Email **security@scimux.com**. Please don't open a public issue for a security
bug — the repository's issue tracker is public, and so is everything in it.

Useful to include, if you have it: what an attacker gains, the steps to
reproduce, the scimux version (the burger menu's About section, or the
commit), and which agent CLI was wrapped. A rough report you are unsure about is still worth
sending; a report nobody sends because it wasn't polished is the worse
outcome.

You'll get a reply from a person within **5 working days**.

scimux is maintained by one person in their own time, and that shapes what is
worth promising: the acknowledgement is a commitment, a fix date is not — you
will always be told which of the two you are waiting for. If you have a
disclosure deadline, say so in your first mail and you'll hear straight away
whether it is realistic.

If five working days pass with no reply, assume the mail went astray rather
than that it was ignored. Resending is welcome, and so is an issue saying only
that you are waiting on a security mail — no details, no reproduction. That
one line is the only thing the public tracker is safe for here, and it reaches
the maintainer by a route that doesn't depend on email.

## Supported versions

Only the latest release and `main` are supported. scimux is pre-1.0: fixes
land on `main` and in the next release, and there are no backports to earlier
tags. (No release number is named here on purpose — this file would go stale
at the next tag, and "the latest release" stays true.)

## What is in scope

- **The browser boundary.** Anything that reaches the controller from a page
  the operator merely has open — a trusted-`Host` or `Sec-Fetch` check that
  can be bypassed, DNS rebinding, framing.
- **Remote access by invite** (`internal/remote`): pairing, invite handling,
  the peer-to-peer data channel, and the bootstrap loader. The wire format
  itself is specified in `scimux-connect`, which delivers the trusted viewer.
  The `scimux-rv` repository owns rendezvous and its own security policy.
  The official pairing link opens the independently hosted trusted viewer at
  `my.scimux.com`; authenticated rendezvous and STUN use `rv.scimux.com`.
  Compromise of the rendezvous may disrupt service and expose its necessary
  metadata, but must not let it supply viewer executable code or authenticate
  as the paired computer. The viewer host and its release path remain trusted.
- **Approvals.** A way to get an agent's tool call approved that the operator
  did not approve — including anything that defeats the one-turn lease, the
  nonce, or the `prompt_id` fence in the Claude permission hook.
- **What scimux launches.** Injection into a launched pane's argv, or a key
  reaching a pane outside the remote-key whitelist.
- **Secrets on disk.** The hook bundles under `~/.scimux/claude-hooks/`, the
  invite file, `settings.json`, and the session logs — anything that makes one
  of these readable or writable by someone who shouldn't have it.

## What is not a vulnerability

- **The unauthenticated local UI.** scimux listens on loopback and has no
  authentication: anyone who can reach the port can read your conversations
  and answer your agents' prompts. That is a documented non-goal: reaching
  scimux from elsewhere is a separate, explicit step — an SSH tunnel, or
  remote access by invite — see "Remote access and security" in the
  [README](README.md). A way to reach that port *around* the Host and
  `Sec-Fetch` checks is in scope; the absence of a login is not.
- **Binding `-addr` wider than loopback.** Doing so exposes full controller
  access by design, and the README says so.
- **Bugs in the wrapped agent CLIs** (claude, codex, pi, opencode, grok,
  cursor-agent, dsh).
  Report those to their vendors. Bugs in how *scimux* drives them are in
  scope.
- **Findings that assume the attacker already has your OS account**, unless
  they cross a boundary scimux does claim — the 0600 hook bundle, the invite
  file, or another user on a shared machine reading either.

## Disclosure

Coordinated: we'd like a fix to exist before the report is public, and we'll
agree a date with you rather than impose one. If the finding is already public
or being exploited, say so — that changes the order things happen in.

There is no bug bounty. Credit in the release notes and the commit is offered
by default, and withheld if you'd rather stay anonymous.
