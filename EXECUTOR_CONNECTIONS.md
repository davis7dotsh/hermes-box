# Executor connections

Executor is installed by default and exposed only on host loopback. Hermes
connects to one reviewed Executor MCP endpoint; provider credentials,
integrations, and policies remain in Executor.

```text
Hermes -> Executor MCP -> provider connection -> provider API
```

Open the portal:

```bash
./bin/hermes-box open executor
```

Create the first admin account, configure provider integrations, and create a
destination-local API token. Provider dashboards and OAuth screens change, so
follow current official provider documentation rather than repository-owned
click paths.

## Human secret boundary

An agent may prepare non-secret fields and navigate to a provider's credential
or consent screen. The user must personally enter or approve API keys, client
secrets, passwords, passkeys, OTPs, OAuth consent, account selection, and
CAPTCHAs.

Never put credentials in chat, shell arguments, history, screenshots, logs,
repository files, or reports.

Register Executor with Hermes:

```bash
./bin/hermes-box setup executor
```

The command reads the token from a no-echo prompt. For automation, send it on
stdin without exposing it in the process list:

```bash
printf '%s' "$EXECUTOR_TOKEN" | \
  ./bin/hermes-box setup executor --token-stdin
```

Hermes Box verifies that the authenticated MCP endpoint exposes only the
reviewed `execute` and `resume` tools, writes the token to Hermes' protected
environment over stdin, restarts Hermes, and verifies discovery.

## Policy checklist

For every provider:

1. Select the intended project, organization, workspace, and account.
2. Request the narrowest credential scopes that support the use case.
3. Block broad provider namespaces by default.
4. Allow only required reads and explicitly reviewed mutations.
5. Keep destructive and administrative operations blocked.
6. Perform one harmless real read through Executor.
7. Confirm one representative blocked mutation remains unavailable.
8. Start a fresh Hermes session and repeat the harmless read through Hermes.

Executor policy limits agent tool use but does not make its credential store a
security boundary from the passwordless-sudo `agent` user. Use another VM when
credentials must be hidden from the agent itself.

Executor's database and provider configuration live only in `/data/executor`.
Release qualification writes a harmless sentinel there and proves that it
survives three stop/start cycles, an encrypted cross-destination restore, and a
root rebuild. It also verifies that the host listener remains loopback-only.
