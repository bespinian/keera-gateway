# Single sign-on

Without an identity provider, the control panel accepts one shared credential:
`KEERA_OPERATOR_KEY`. Every audit entry then reads `operator key`, so it does
not say who did what.

With SSO, the audit log names a person, and roles can differ between people.

Keera uses standard OpenID Connect, so any compliant provider works. This page
covers **Google Workspace** and **Microsoft Entra ID**. A deployment can offer
one or both.

## One directory or several

| Shape                  | Identity                                | Configured with                           |
| ---------------------- | --------------------------------------- | ----------------------------------------- |
| Dedicated, on-premises | the customer's own directory            | `KEERA_OIDC_ISSUER` and the two beside it |
| Hosted                 | whichever directory each customer is on | `KEERA_OIDC_PROVIDERS`                    |

With one directory, the panel shows "Continue with single sign-on". The
unprefixed `KEERA_OIDC_*` variables configure it.

With several, the panel shows one named button per directory. See
[Several directories at once](#several-directories-at-once).

## Where a role comes from

### The operator role comes from the environment, always

An address in `KEERA_OPERATORS`, or a group in `KEERA_OIDC_OPERATOR_GROUPS`,
grants it. Nothing else does: the panel does not offer it, `keera user role`
refuses it, and `PATCH /control/v1/users/{id}` answers 403, even to an operator.

The operator role spans organisations and owns the model catalogue, so you can
see who holds it by reading the configuration. Remove an address from
`KEERA_OPERATORS`, or a person from the group, and they are demoted at their
next sign-in.

### The administrator role comes from one of two places

| `KEERA_OIDC_ADMIN_GROUPS` | An administrator is                                  | Changed in                                                                      |
| ------------------------- | ---------------------------------------------------- | ------------------------------------------------------------------------------- |
| set                       | whoever is in the group                              | the directory                                                                   |
| unset                     | whoever an operator or another administrator says so | `keera user role`, the panel's **Users** screen, `PATCH /control/v1/users/{id}` |

**If set, the directory decides.** The group is read on every sign-in, so a
person removed from it is demoted at their next sign-in. The panel _no longer
lets you change roles_, because the next sign-in would undo the change. With
several directories, admin groups on one provider are enough to make the
directory decide for the whole gateway.

**If unset, Keera decides.** Everyone starts with `KEERA_OIDC_DEFAULT_ROLE`, and
an operator or administrator promotes them:

```sh
keera user role alice@example.ch admin
```

The change is audited, signs the person out everywhere so the new role applies
at once, and stays until someone changes it. You can also add a person before
their first sign-in with `keera user add alice@example.ch --role admin`; their
identity takes over that row when they first sign in.

This is the only option on Google Workspace.

### What each provider does and does not give you

**Google sends no groups claim.** Workspace groups are only available through
the Admin SDK, which Keera does not call. So the group lists match nothing on
Google. Leave both unset.

Setting `KEERA_OIDC_ADMIN_GROUPS` on Google leaves you with no administrators
and no way to make one: the directory never grants the role, and the panel can
no longer grant it. The gateway warns about this at start-up.

**Entra sends groups as object GUIDs** (`11111111-2222-…`, not `keera-admins`),
unless the group was synced from Active Directory with a `sAMAccountName`. For a
person in too many groups, Entra sends no groups at all, only a pointer to
Microsoft Graph. Keera does not call Graph, so this looks like a person in no
groups. It usually hits administrators, who are in the most groups.

Keera refuses that sign-in instead of signing an administrator in as a member,
but only when a group would decide the role. **App roles avoid the problem**,
and the walkthrough below uses them.

## Google Workspace

1. Pick or create a project, then open **APIs & Services → OAuth consent
   screen**.
2. Choose **Internal** as the user type. This limits sign-in to your Workspace
   and stops the fallback in
   [Which tenant a sign-in lands in](#which-tenant-a-sign-in-lands-in) from
   admitting any Google account. **External** would let any Google account
   reach the panel.
3. Under **Scopes**, add `openid`, `email` and `profile`, the three Keera asks
   for when `KEERA_OIDC_SCOPES` is unset.
4. **Credentials → Create credentials → OAuth client ID**, type **Web
   application**.
5. Add one **Authorised redirect URI**:

   ```
   http://localhost:8080/control/auth/callback
   ```

   Google matches it exactly. It allows `http` only for `localhost`, which is
   why the compose deployment works without TLS. It refuses `127.0.0.1` for a
   Web application client, so open the panel at `localhost:8080`.

6. Copy the client ID and client secret.

In `compose/.env`:

```sh
KEERA_OIDC_ISSUER=https://accounts.google.com
KEERA_OIDC_CLIENT_ID=<from the console>
KEERA_OIDC_CLIENT_SECRET=<from the console>
KEERA_OIDC_REDIRECT_URL=http://localhost:8080/control/auth/callback
KEERA_PUBLIC_URL=http://localhost:8080
KEERA_OIDC_DEFAULT_ROLE=member
KEERA_OPERATORS=you@example.ch
```

The issuer is the bare host; the discovery path is added for you. Discovery runs
once at start-up, so a typo stops the boot with the URL in the error.

There are no group variables on Google. The first operator is the address in
`KEERA_OPERATORS`. Make administrators with `keera user role` or on the panel's
Users screen.

## Microsoft Entra ID

1. **Microsoft Entra admin centre → App registrations → New registration.**
2. Under **Supported account types**, choose **Accounts in this organizational
   directory only**. This is the same boundary as Google's **Internal**. The
   multi-tenant options admit accounts from other directories, and Keera places
   a first sign-in by its email domain.
3. **Redirect URI**: platform **Web**, and the absolute URL of the callback:

   ```
   https://keera.example.ch/control/auth/callback
   ```

   Entra allows `http` only for `localhost`, like Google.

4. **Certificates & secrets → New client secret.** Copy the _value_, not the
   secret ID. The value is shown only once. Note the expiry: Entra secrets last
   at most 24 months, and an expired secret fails every sign-in.
5. **Token configuration → Add optional claim**, token type **ID**, and add
   `email`. Entra does not always put an email in the ID token, and Keera
   refuses an identity without one.
6. Roles, if the directory should decide them instead of Keera. Use **App
   roles**, not groups:

   **App roles → Create app role**, with **Users/Groups** as the allowed member
   type and the value `keera-admins`. Repeat with `keera-operators` if you want
   that role too. Then assign people or groups under **Enterprise applications →
   your app → Users and groups**.

   App roles arrive by name in the `roles` claim and are never replaced by an
   overage pointer. Group object IDs also work (put the GUIDs in the group lists
   and keep the claim as `groups`), but read
   [What each provider does and does not give you](#what-each-provider-does-and-does-not-give-you)
   first.

   To assign administrators in Keera instead, skip this step and leave
   `KEERA_OIDC_ADMIN_GROUPS` out of the block below.

Then:

```sh
KEERA_OIDC_ISSUER=https://login.microsoftonline.com/<tenant-id>/v2.0
KEERA_OIDC_CLIENT_ID=<application (client) ID>
KEERA_OIDC_CLIENT_SECRET=<the secret value>
KEERA_OIDC_REDIRECT_URL=https://keera.example.ch/control/auth/callback
KEERA_PUBLIC_URL=https://keera.example.ch
KEERA_OIDC_GROUPS_CLAIM=roles
KEERA_OIDC_ADMIN_GROUPS=keera-admins
KEERA_OIDC_OPERATOR_GROUPS=keera-operators
KEERA_OIDC_DEFAULT_ROLE=member
```

The issuer must contain the tenant ID and end in `/v2.0`. The v1 endpoint issues
tokens whose `iss` does not match what discovery reports.

> **Not tested against a real tenant.** This setup follows the protocol and
> Entra's documentation. The group-overage refusal has a test.

## Several directories at once

This is the hosted deployment. List the providers, then give each its own
client:

```sh
KEERA_OIDC_PROVIDERS=google,entra

KEERA_OIDC_GOOGLE_ISSUER=https://accounts.google.com
KEERA_OIDC_GOOGLE_CLIENT_ID=<from the Google console>
KEERA_OIDC_GOOGLE_CLIENT_SECRET=<from the Google console>

KEERA_OIDC_ENTRA_ISSUER=https://login.microsoftonline.com/<tenant-id>/v2.0
KEERA_OIDC_ENTRA_CLIENT_ID=<from the Entra admin centre>
KEERA_OIDC_ENTRA_CLIENT_SECRET=<from the Entra admin centre>
KEERA_OIDC_ENTRA_GROUPS_CLAIM=roles
KEERA_OIDC_ENTRA_ADMIN_GROUPS=keera-admins

# Shared by every provider above.
KEERA_OIDC_REDIRECT_URL=https://keera.example.ch/control/auth/callback
KEERA_OIDC_DEFAULT_ROLE=member
KEERA_OPERATORS=you@example.ch
```

Every setting except the issuer, client ID and client secret falls back to its
unprefixed name. To override one for a provider, use
`KEERA_OIDC_<NAME>_<SETTING>`. The issuer and client are never inherited: two
providers sharing a client would sign people in against the wrong directory
instead of failing.

`KEERA_OIDC_ENTRA_ADMIN_GROUPS` is set per provider, but it applies to the
**whole gateway**, Google users included. Google sends no groups, so nobody
signing in through Google could ever become an administrator. If one of your
customers uses Google, leave that line out.

**One callback serves every provider.** Keera recognises a sign-in by its state,
not by the callback address. Each provider's client registration must still list
the callback.

A **name** matches `[a-z0-9][a-z0-9-]*`. Do not rename a provider: the name is
part of the sign-in URL and of every identity it creates, so renaming it cuts
off everyone who signed in through it. The button label comes from the name
(`google` shows Google; `entra`, `azure`, `azuread`, `microsoft` and `m365` show
Microsoft) or from `KEERA_OIDC_<NAME>_LABEL`.

### One person, one provider

An identity is stored as `<provider>:<subject>`, because a subject is only
unique within its directory.

A row an administrator created in advance has no subject yet. The first sign-in
with a matching address takes it over. A row that already has _another
provider's_ subject is refused instead. Otherwise anyone who can get an address
through the weaker directory could take over the account and role that the
stronger one vouched for.

`KEERA_OIDC_ADOPT_BY_EMAIL=true` turns the refusal off. Use it only while moving
an organisation from one provider to another, then turn it off again.

## Which tenant a sign-in lands in

`orgFor` in `internal/control/auth.go` checks in this order:

1. A user already linked to that provider's subject.
2. An organisation whose `email_domain` matches the address's domain.
3. The only organisation, if there is exactly one.
4. Otherwise refuse.

On a single-org compose deployment, step 3 applies. So **every** directory user
who passes the consent screen joins that org with the default role. Restrict
the consent screen to your own directory to keep that to your own staff.

**On the hosted deployment, avoid step 3.** It stops applying once there is a
second organisation, but until then a new customer's first sign-in lands in the
existing tenant. Give every org its domain before you create the second one:

```sh
keera org create "Another Bank" --domain anotherbank.ch
keera org set <org-id> --domain yourdomain.ch   # the one that was there first
```

You can leave out the id while there is only one organisation. `keera org list`
shows each domain. Domains are unique: a second organisation claiming the same
one is refused.

Setting a domain moves nobody. It is only read on a first sign-in, so existing
users stay where they are. `keera org set <org-id> --no-domain` removes it.

The same through the control API:

```sh
curl -X PATCH https://keera.example.ch/control/v1/orgs/<org-id> \
  -H "Authorization: Bearer $KEERA_OPERATOR_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"email_domain":"yourdomain.ch"}'
```

## Signing in

The panel shows one button per provider. With an identity provider configured,
the operator key is no longer on the sign-in screen. It still works, and it is
the way back in if SSO is broken: open `/?operator_key=1`.

## Signing in from the command line

`keera login` is the same sign-in for the terminal:

```sh
keera login --url https://keera.example.ch    # --provider, where there are several
```

It opens a browser, the directory signs the person in as for the panel, and
the command gets a credential for that person. Commands then run as them: their
role decides what is allowed, `keera usage` shows their organisation, and the
audit log records their address.

The credential is written to `~/.config/keera/credentials.json` (mode 0600),
one entry per gateway. `KEERA_CONFIG_DIR` moves the file. The gateway you signed
in to becomes the default for later commands, so nothing needs to go in a shell
profile.

You can be signed in to several deployments at once. A command picks the
gateway in this order: `--url`, then `KEERA_CONTROL_URL`, then the gateway last
signed in to, then the built-in default `https://gateway.keera.ch`.
`keera whoami` shows which address is used and why. `keera logout` also clears
the default if it pointed at the removed sign-in.

The credential lasts 30 days. `keera logout` ends it sooner,
`keera logout --all` ends every sign-in of the account everywhere, and
`keera whoami` shows which credential on this machine is in use.

**`KEERA_OPERATOR_KEY` still wins where it is set**, since someone exported it
on purpose. `keera login` warns when it finds one. The operator key is still the
right credential for automation, pipelines and the first minutes of a
deployment.

### What it does, and what makes that safe

It uses the loopback redirect from RFC 8252, like `gcloud` and `gh`:

1. `keera login` listens on a port on 127.0.0.1 and opens `/control/auth/login`
   with that address, a random state, and the SHA-256 of a proof key it keeps.
2. The identity provider signs the person in as usual. No new redirect URI is
   registered: the panel's callback is reused.
3. The callback redirects the browser to the loopback port with a one-time code.
4. `keera login` redeems the code, with the proof key, for a token.

Two rules make it safe:

- The redirect must be a literal loopback address (`127.0.0.1` or `[::1]`, with
  a port, over `http`), or the sign-in is refused. The name `localhost` is
  refused too, because it can resolve to anything.
- The code is useless without the proof key. Any process on the machine can bind
  a loopback port, but only the process that started the sign-in has the proof
  key. The code is deleted when redeemed.

A role change signs out the terminal as well as the browser.

### On a machine with no browser

The sign-in URL is printed as well as opened, so on a host without a desktop you
can copy and paste it. `--no-browser` skips opening the browser.

**Over ssh, copy and paste is not enough.** The loopback listener is on the
remote machine and the browser is on yours. Forward the printed port with
`ssh -L`, or sign in on a machine with a browser and copy the credentials file
across. If you need this often, use the operator key instead.

## Moving off localhost

Put the gateway's port behind a TLS terminator, then change three things: the
authorised redirect URI in each provider's console, `KEERA_OIDC_REDIRECT_URL`
and `KEERA_PUBLIC_URL`.

`KEERA_SECURE_COOKIES` follows the scheme, so the session cookie becomes
`Secure` without another setting.

## When someone leaves

Removing a person from the directory stops their next sign-in, but not their API
keys. The directory never sees those. So disable them in Keera as well:

```sh
keera user disable ada@example.ch
```

Or use **Users → Disable** in the panel. Immediately:

- they cannot sign in, and all their sessions and `keera login` tokens end
- every key attributed to them is revoked for good
- their agent sandboxes are terminated, and their own are suspended with the
  volume kept, so an administrator can decide what to do with the work
- the repository credentials those sandboxes held are revoked

Usage, sandboxes and audit entries keep their name. `keera user enable` lets
them back in without keys: the old ones stay revoked. An administrator can
disable anyone in their organisation except themselves and an operator.

Keys attributed to nobody, such as a build pipeline's, are not affected. Give a
key a `--user` when a person is behind it.

## When it does not work

| Symptom                                                            | Cause                                                                                                                                                  |
| ------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Gateway will not start, "discovering the identity provider"        | An issuer is wrong, or the container cannot reach it. The message names which one.                                                                     |
| `redirect_uri_mismatch`                                            | The console URI and `KEERA_OIDC_REDIRECT_URL` differ, often `127.0.0.1` against `localhost`.                                                           |
| No sign-on button on the panel                                     | An issuer and a client ID are both needed; one is empty.                                                                                               |
| Boot fails asking for a client secret                              | An issuer and client ID are set without the secret. The message names the provider.                                                                    |
| "a sign-in has to name one of…"                                    | Several providers are configured and the link named none. Start from the panel, not the URL.                                                           |
| Signed in, but everything is read-only                             | You have the default role. Ask an administrator for `keera user role <you> admin`, or, if `KEERA_OIDC_ADMIN_GROUPS` is set, to be added to that group. |
| The panel will not let anybody change a role                       | `KEERA_OIDC_ADMIN_GROUPS` is set on some provider, so the directory decides. Change the group, or unset it on every provider.                          |
| The panel refuses the operator role                                | It always does. `KEERA_OPERATORS` or `KEERA_OIDC_OPERATOR_GROUPS` grants it.                                                                           |
| "no organisation matches …"                                        | There are two or more orgs and none has your email domain. Set it, as above.                                                                           |
| "that email domain belongs to another organisation"                | Each domain belongs to one tenant. `keera org list` shows which; clear it there first.                                                                 |
| "in too many groups"                                               | Entra group overage. Use app roles, or grant the role by address. See above.                                                                           |
| "already belongs to an account from a different identity provider" | That address signed in through another provider first. See [One person, one provider](#one-person-one-provider).                                       |
| Entra: "returned no email claim"                                   | Add `email` as an optional ID-token claim on the app registration.                                                                                     |
| `keera login`: "has no identity provider configured"               | The gateway has none. Use `KEERA_OPERATOR_KEY`, or configure one as above.                                                                             |
| `keera login`: "has to come back to a loopback address"            | The command line is older than the gateway, or something rewrote its redirect. Rebuild it.                                                             |
| `keera login`: the browser signs in and nothing happens            | The browser is not on the same machine as the command, usually because of ssh. See above.                                                              |
| A command says "your sign-in is no longer valid"                   | It expired, a role change ended it, or someone ran `keera logout --all`. Run `keera login` again.                                                      |
| Entra: "did not match the issuer URL returned by provider"         | The issuer is `.../common/v2.0`, which answers discovery with a templated `{tenantid}` that matches nothing. Use the tenant ID.                        |
