# Executor Connection and Credential Guide

Use this guide when adding an external service to Executor and making it
available to Hermes. It focuses on the part that usually takes the most time:
finding the correct credential in each provider dashboard, choosing the right
authentication method, and proving that the resulting connection is both
useful and safely constrained.

Provider dashboards change over time. Treat the paths below as navigation
landmarks, verify the live labels, and prefer the provider's official
documentation if the UI has moved.

## Operating Model

Hermes connects to Executor once. Individual provider accounts, credentials,
integrations, and policies remain in Executor.

```text
Hermes -> Executor MCP -> provider connection -> provider API
```

Before changing a connection, inspect the current state:

```bash
./bin/hermes-box executor status
./bin/hermes-box executor auth status
./bin/hermes-box executor connections --json
./bin/hermes-box executor open
```

Executor integrations normally take one of three forms:

- Built-in preset, such as Google
- Official OpenAPI or Discovery document, such as X or YouTube Data
- Hosted MCP connection, such as Notion

Creating an integration defines the available tool surface. Creating a
connection supplies the credential or OAuth grant for one account. One
integration can have multiple named account connections.

## Secret and Browser Handoff

An agent may navigate to the provider dashboard, prepare non-secret fields,
create or import an integration, and focus the credential input. The user must
personally enter or approve:

- API keys, bearer tokens, client secrets, PATs, and bot tokens
- Passwords, passkeys, OTPs, and two-factor authentication
- OAuth consent and account, workspace, organization, or server selection
- CAPTCHAs and password-manager prompts

The agent should stop at that boundary and say:

> Enter the requested credential in the focused field, complete any consent or
> two-factor authentication, and tell me when it is done. Do not paste the
> credential into chat.

Never place a credential in chat, a shell argument, shell history, a
screenshot, a log, a repository file, or a saved report. The host-side Executor
API key belongs in the per-machine macOS Keychain via:

```bash
./bin/hermes-box executor auth set
```

## Google Cloud API Key

Use an API key for public YouTube Data API requests such as public channels,
videos, playlists, search results, comments, view counts, and engagement
figures.

1. Open the [Google Cloud Console](https://console.cloud.google.com/).
2. Select the intended project. Confirm the project before creating anything;
   credentials and quotas belong to that project.
3. Open **APIs & Services -> Library**.
4. Find and enable **YouTube Data API v3**.
5. Open **APIs & Services -> Credentials**.
6. Select **Create credentials -> API key**.
7. Open the new key and add an API restriction for **YouTube Data API v3**.
8. Add an application restriction only when Executor has a stable source IP or
   another restriction that will not break the VM after moving or restoring it.
9. Copy the key and enter it directly into Executor.

For the YouTube Data API integration, configure:

```text
Authentication: API key
Location:       Query parameter
Parameter:      key
```

The official Discovery document is:

```text
https://www.googleapis.com/discovery/v1/apis/youtube/v3/rest
```

This is public YouTube **Data** access. Google's specifically named YouTube
Analytics and Reporting APIs expose private channel-owner reports and require
OAuth 2.0. An API key alone is not sufficient for those APIs.

References:

- [Manage Google Cloud API keys](https://docs.cloud.google.com/docs/authentication/api-keys)
- [YouTube Data API authorization credentials](https://developers.google.com/youtube/registering_an_application)
- [YouTube Analytics authorization credentials](https://developers.google.com/youtube/reporting/guides/registering_an_application)

## Google OAuth Client ID and Secret

Use OAuth for Gmail, Calendar, Drive, Docs, Sheets, private YouTube data, or
YouTube Analytics.

1. Select the intended Google Cloud project.
2. Under **APIs & Services -> Library**, enable every API the Executor
   integration will use.
3. Configure the OAuth consent/branding screen if the project does not already
   have one.
4. Open **APIs & Services -> Credentials**.
5. Reuse an appropriate existing web client or select
   **Create credentials -> OAuth client ID**.
6. Choose **Web application**.
7. Copy the callback shown by Executor into **Authorized redirect URIs**
   exactly, including scheme, hostname, port, path, and any trailing slash.
8. Preserve existing redirect URIs unless their removal is explicitly in
   scope.
9. Copy the Client ID and Client Secret into Executor's OAuth app form.
10. Use the Authorization Code flow and complete consent in the same browser.
11. Select the intended Google account and verify that the connection label
    clearly identifies it.

A local Hermes Box currently displays a callback shaped like:

```text
http://localhost:4788/api/oauth/callback
```

Do not copy that example blindly. Use the exact callback displayed by the live
Executor instance.

Google presets can request broad OAuth scopes. Executor policy constrains what
the agent can invoke, but it does not reduce the underlying token's power if
the credential is copied elsewhere. Apply policy immediately after connecting.

References:

- [Google OAuth for web-server applications](https://developers.google.com/identity/protocols/oauth2/web-server)
- [YouTube Analytics OAuth authorization](https://developers.google.com/youtube/reporting/guides/authorization/server-side-web-apps)

## X Bearer Token

Use a bearer token for supported app-only, public-data endpoints.

1. Open the [X Developer Console](https://console.x.com/).
2. Create or select the intended app.
3. Open the app's **Keys and tokens** page.
4. Find **Bearer Token** and generate or regenerate it.
5. Save it immediately in the focused Executor credential field. X may only
   display it once, and regeneration invalidates the previous token.

Configure the Executor authentication method as:

```text
Type:    API key
Header:  Authorization
Prefix:  Bearer
```

For a full X API v2 integration, the official OpenAPI document is:

```text
https://api.twitter.com/2/openapi.json
```

The full specification includes mutations and private operations. Set a broad
namespace block first, then allow only the intended public read/search tools.
Do not assume that every imported analytics or archive-search operation is
available to the bearer token or the current X API plan. Prove access with a
real, harmless request.

References:

- [X developer apps and credentials](https://docs.x.com/fundamentals/developer-apps)
- [Getting X API access](https://docs.x.com/x-api/getting-started/getting-access)

## X OAuth Client ID and Secret

Use OAuth 2.0 user context when Executor must access account-specific data or
act as an X user. A public bearer token is insufficient for those operations.

1. In the X Developer Console, create or select the app.
2. Configure user authentication.
3. Choose **Web App** or **Automated App / Bot** when Executor needs a
   confidential client with a Client Secret.
4. Add the exact Executor callback URL.
5. Request only the necessary scopes. A read-only starting point is:

   ```text
   tweet.read users.read offline.access
   ```

6. Open **Keys and tokens** and copy the Client ID and Client Secret into the
   focused Executor OAuth app fields.
7. Complete the OAuth consent flow in the same browser.

Use these endpoints if Executor asks for them explicitly:

```text
Authorization URL: https://x.com/i/oauth2/authorize
Token URL:         https://api.x.com/2/oauth2/token
```

The API-hosted authorization URL sometimes inferred from an OpenAPI document
can return a browser 403. The interactive authorization endpoint is on
`x.com`, while token exchange remains on `api.x.com`.

References:

- [X OAuth 2.0 Authorization Code flow](https://docs.x.com/fundamentals/authentication/oauth-2-0/authorization-code)
- [X authentication overview](https://docs.x.com/resources/fundamentals/authentication/overview)

## Discord Bot Token

Use a dedicated bot rather than a personal Discord credential.

1. Open the [Discord Developer Portal](https://discord.com/developers/applications).
2. Create or select the intended application.
3. The **General Information** page contains the Application ID and Public Key;
   these are identifiers, not the bot credential.
4. Open **Bot**.
5. Under **Token**, choose **Reset Token** and copy the newly generated token
   directly into Executor. Discord does not reveal it again without another
   reset.
6. Enable **Message Content Intent** only when the integration must read message
   bodies.
7. Open **Installation** and configure the bot scope and only the required
   server permissions.
8. Install the bot into the intended server.

Executor header authentication uses:

```text
Header: Authorization
Value:  Bot <token>
```

For a read-only integration, begin with **View Channels** and
**Read Message History**. Avoid Send Messages, Manage Messages, Manage Channels,
Manage Roles, Manage Webhooks, and Administrator unless the use case explicitly
requires them.

References:

- [Discord bot setup](https://docs.discord.com/developers/docs/getting-started)
- [Discord Gateway intents](https://docs.discord.com/developers/topics/gateway)

## Airtable Personal Access Token

1. Open Airtable's Developer Hub.
2. Select **Personal access tokens** under **Developers**.
3. Select **Create token** and use a name that identifies Executor and the
   environment.
4. Add only the required scopes.
5. Select only the required bases or workspace resources.
6. Create the token and copy it directly into Executor.

Use standard bearer authentication unless the selected adapter specifies
otherwise:

```text
Header: Authorization
Prefix: Bearer
```

Prefer a PAT limited to one base over an organization-wide token.

Reference: [Create an Airtable personal access token](https://support.airtable.com/docs/creating-personal-access-tokens)

## GitHub Fine-Grained Personal Access Token

Use a GitHub App for a long-lived multi-user integration. For a narrow personal
connection, prefer a fine-grained PAT over a classic PAT where supported.

1. On GitHub, open the profile menu and select **Settings**.
2. Open **Developer settings -> Personal access tokens -> Fine-grained
   tokens**.
3. Select **Generate new token**.
4. Choose the intended resource owner and an expiration date.
5. Select only the necessary repositories.
6. Grant the minimum repository and organization permissions required by the
   desired Executor tools.
7. Generate the token and copy it directly into Executor.

Use:

```text
Header: Authorization
Prefix: Bearer
```

Reference: [Managing GitHub personal access tokens](https://docs.github.com/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens)

## Notion Hosted MCP

Notion's hosted MCP normally does not require hunting for a manually copied
credential.

1. Add or select the hosted Notion MCP integration in Executor.
2. Select **Connect**.
3. Complete Notion OAuth in the browser.
4. Select the intended workspace.
5. Give each connection an unambiguous account/workspace label.
6. Repeat the OAuth flow for additional workspaces.

The hosted MCP URL is:

```text
https://mcp.notion.com/mcp
```

If the dashboard offers OAuth directly, use it instead of creating an internal
integration token unnecessarily.

## Policy and Verification

Connecting successfully is not the finish line. Apply policy and verify both
the allowed and denied surfaces.

Recommended default:

1. Block the broad provider namespace.
2. Add narrow exceptions for intended reads.
3. Use approval-required rules for mutations that are genuinely needed.
4. Keep destructive or administrative operations blocked.

For broad presets such as Google, it can be more practical to preserve reads
and add exact mutation blocks. Inspect the live generated tool names rather
than copying a stale allowlist from an older Executor version.

Verify in this order:

1. The named account appears in Executor's connection list.
2. The expected namespace and tools are discoverable.
3. One harmless real provider read returns expected data.
4. One representative mutation is hidden or blocked.
5. Executor's MCP endpoint passes its protocol test.
6. A fresh Hermes session can perform the harmless read through Executor.

```bash
./bin/hermes-box executor connections --json
./bin/hermes-box executor tools --namespace <namespace> --json
./bin/hermes-box executor mcp-test
```

Connection and policy changes are dynamic. Restart Hermes only when its MCP or
gateway configuration changes. When registering Executor with Hermes for the
first time, use:

```bash
./bin/hermes-box executor connect-hermes
```

Then start a fresh Hermes session so it discovers the new MCP tools.

## Connection Record

For each finished connection, record only non-secret evidence:

```text
Provider:
Provider project/app/workspace:
Integration display name:
Executor namespace:
Connection label and normalized slug:
Authentication type and placement:
Enabled APIs or OAuth scopes:
Imported tool count:
Policy summary:
Successful harmless read:
Confirmed blocked mutation:
Executor MCP test:
Hermes end-to-end test:
Date and operator:
```

Do not record the credential value. Executor state under
`/workspace/executor/data` and Hermes Box snapshots or portable packages can
contain live credentials. Treat those artifacts as credential vaults and
encrypt them at rest.
