# Single sign-on

Without an identity provider, the control panel accepts one shared credential:
`KEERA_OPERATOR_KEY`. Every audit entry then reads `operator key`, or
`operator@localhost` for a panel sign-in with it, so it does not say who did
what.

With SSO, the audit log names a person, and roles can differ between people.

Keera uses standard OpenID Connect, so any compliant provider works. This page
covers **Google Workspace**, **Microsoft Entra ID** and **GitHub**, which speaks
plain OAuth and gets its own handling, and **LDAP and Active Directory**, which
sign in with a password. A deployment can offer any of them. For people no
directory vouches for, it can also offer [passkeys](#passkeys).

How to sign in and manage people:
[Single sign-on and passkeys](https://docs.keera.ch/operate/sign-on/),
[Sign in](https://docs.keera.ch/use/sign-in/) and
[Users and roles](https://docs.keera.ch/manage/users/). This page explains how
it works and why.

## Naming a provider

Every provider has a name, listed in `KEERA_OIDC_PROVIDERS`. It reads its
settings from `KEERA_OIDC_<NAME>_*`, with the name in upper case and a hyphen
as an underscore: `acme-ad` reads `KEERA_OIDC_ACME_AD_*`. Nothing is shared
between providers. A name in the list without its `ISSUER`, `CLIENT_ID` and
`CLIENT_SECRET` stops the start, except an
[LDAP directory](#ldap-and-active-directory), which needs no client. The
settings for each provider are in
[Set up a provider](https://docs.keera.ch/operate/sign-on/#set-up-a-provider).

The identity provider sends the browser back to
`<KEERA_PUBLIC_URL>/control/auth/callback`, so SSO needs `KEERA_PUBLIC_URL`. The
gateway refuses to start without it as soon as `KEERA_OIDC_PROVIDERS` names a
provider. Register that address as the redirect URI with each provider, except
an LDAP directory, which sends the browser nowhere.

### Settings

Discovery runs at start-up, so a wrong issuer URL stops the gateway with the URL
in the error, instead of breaking sign-in later. Lists, such as addresses,
groups, domains and scopes, are comma-separated. An LDAP directory's groups are
separated by `;`, since a DN holds commas itself.

| Variable                            | Default                                                 | What it is                                                                                                                                                                                 |
| ----------------------------------- | ------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `KEERA_OIDC_PROVIDERS`              | unset                                                   | The provider names, comma-separated.                                                                                                                                                       |
| `KEERA_OPERATORS`                   | unset                                                   | The addresses that are operators, whichever provider signs them in. See [Where a role comes from](#where-a-role-comes-from).                                                               |
| `KEERA_OIDC_<NAME>_ISSUER`          | unset                                                   | The provider's issuer URL.                                                                                                                                                                 |
| `KEERA_OIDC_<NAME>_CLIENT_ID`       | unset                                                   | The OAuth client registered with the provider.                                                                                                                                             |
| `KEERA_OIDC_<NAME>_CLIENT_SECRET`   | unset                                                   | Its secret.                                                                                                                                                                                |
| `KEERA_OIDC_<NAME>_LABEL`           | from the name, such as `Google`                         | The text on the sign-in button.                                                                                                                                                            |
| `KEERA_OIDC_<NAME>_SCOPES`          | `openid,profile,email`, and `offline_access` off Google | What Keera asks the provider for. See [Asking the directory again](#asking-the-directory-again), and [GitHub](#github) for its scopes.                                                     |
| `KEERA_OIDC_<NAME>_GROUPS_CLAIM`    | `groups`, and `memberOf` for LDAP                       | The claim that holds the person's groups.                                                                                                                                                  |
| `KEERA_OIDC_<NAME>_ADMIN_GROUPS`    | unset                                                   | Groups whose members are administrators. Set, the directory decides roles. See [Where a role comes from](#where-a-role-comes-from).                                                        |
| `KEERA_OIDC_<NAME>_OPERATOR_GROUPS` | unset                                                   | Groups whose members are operators.                                                                                                                                                        |
| `KEERA_OIDC_<NAME>_DEFAULT_ROLE`    | `member`                                                | `admin` or `member`, for someone in no admin group.                                                                                                                                        |
| `KEERA_OIDC_<NAME>_DOMAINS`         | unset                                                   | The email domains this provider may sign people in with, `*` for any. Needed with several providers ([Several directories](#several-directories-at-once)), and always for GitHub and LDAP. |
| `KEERA_OIDC_<NAME>_SIGNUP`          | `false`                                                 | Lets someone whose sign-in matches no organisation create one. See [Letting people sign up](#letting-people-sign-up).                                                                      |
| `KEERA_OIDC_<NAME>_LDAP_*`          | unset                                                   | The settings of an LDAP directory. See [LDAP and Active Directory](#ldap-and-active-directory).                                                                                            |
| `KEERA_PASSKEYS`                    | `false`                                                 | Lets administrators create accounts that sign in with a passkey. See [Passkeys](#passkeys).                                                                                                |

## Where a role comes from

### The operator role comes from the environment, always

An address in `KEERA_OPERATORS`, or a group in
`KEERA_OIDC_<NAME>_OPERATOR_GROUPS`, grants it, even to someone who is also in
an admin group. Nothing else does: the panel does not offer it,
`keera user role` refuses it, `PATCH /control/v1/users/{id}` answers 403, even
to an operator, and `KEERA_OIDC_<NAME>_DEFAULT_ROLE` cannot be `operator`.

The operator role spans organisations, so you can see who holds it by reading
the configuration. An administrator runs one organisation, including its models,
MCP servers, filters, routers and sandbox classes. Remove a person from the
group, and they are demoted within 15 minutes (see
[Asking the directory again](#asking-the-directory-again)). Remove an address
from `KEERA_OPERATORS`, and they are demoted at the next directory check after
the restart.

### The administrator role comes from one of two places

| `KEERA_OIDC_<NAME>_ADMIN_GROUPS` | An administrator is                                  | Changed in                                                                      |
| -------------------------------- | ---------------------------------------------------- | ------------------------------------------------------------------------------- |
| set                              | whoever is in the group                              | the directory                                                                   |
| unset                            | whoever an operator or another administrator says so | `keera user role`, the panel's **Users** screen, `PATCH /control/v1/users/{id}` |

**If set, the directory decides.** The group is read at every sign-in and, with
a refresh token, [every 15 minutes after](#asking-the-directory-again), so a
person removed from it loses the role. The panel _no longer lets you change roles_, because the next
sign-in would undo the change. With several directories, admin groups on one
provider are enough to make the directory decide for the whole gateway. It
decides only in the organisation the person's address lands in (see [Which
organisation a sign-in lands in](#which-organisation-a-sign-in-lands-in)). In
any other organisation they are in, they keep the role it gave them.

**If unset, Keera decides.** Everyone starts with
`KEERA_OIDC_<NAME>_DEFAULT_ROLE` (`member` unless set to `admin`), and an
operator or administrator promotes them
([Change a role](https://docs.keera.ch/manage/users/#change-a-role)). The
change is audited, signs the person out of this organisation so the new role
applies at once, and stays until someone changes it.

This is the only option on Google Workspace, which
[sends no groups](#what-each-provider-does-and-does-not-give-you).

### What each provider does and does not give you

**Google sends no groups claim.** Workspace groups are only available through
the Admin SDK, which Keera does not call. So the group lists match nothing on
Google. Leave both unset.

Setting admin groups on Google leaves you with no administrators and no way to
make one: the directory never grants the role, and the panel can no longer grant
it. The gateway warns about this at start-up.

**Entra sends groups as object GUIDs** (`11111111-2222-…`, not `keera-admins`),
unless the group was synced from Active Directory with a `sAMAccountName`. For a
person in too many groups, Entra sends no groups at all, only a pointer to
Microsoft Graph. Keera does not call Graph, so this looks like a person in no
groups. It usually hits administrators, who are in the most groups.

Keera refuses that sign-in instead of signing an administrator in as a member,
but only when a group would decide the role. **App roles avoid the problem**,
and the walkthrough below uses them.

**GitHub's groups are organisations and teams**, as `bespinian` and
`bespinian/keera-admins`. Nobody else can create one of the same name. An
organisation that restricts OAuth apps hides itself and its teams until an
owner approves Keera, so a person in it looks like a person in no groups.

## Google Workspace

The steps and settings are in
[Set up a provider](https://docs.keera.ch/operate/sign-on/#set-up-a-provider).
Why they are so:

- **Internal**, as the consent screen's user type, limits sign-in to your
  Workspace and stops the fallback in
  [Which organisation a sign-in lands in](#which-organisation-a-sign-in-lands-in)
  from admitting any Google account. **External** would let any Google account
  reach the panel.
- When `KEERA_OIDC_<NAME>_SCOPES` is unset, Keera asks Google for `openid`,
  `email` and `profile`, and for offline access, so it gets a refresh token.
- Google matches the redirect URI exactly. It allows `http` only for
  `localhost`, which is why the compose deployment works without TLS. It
  refuses `127.0.0.1` for a Web application client, so open the panel at
  `localhost:8080`.
- The issuer is the bare host; the discovery path is added for you.

## Microsoft Entra ID

The steps and settings are in
[Set up a provider](https://docs.keera.ch/operate/sign-on/#set-up-a-provider).
Why they are so:

- **This directory only**, as the supported account types. This is the same
  boundary as Google's **Internal**. The multi-tenant options admit accounts
  from other directories, and Keera places a first sign-in by its email domain.
- **The secret expires.** Copy its _value_, not its ID; it is shown only once.
  Entra secrets last at most 24 months, and an expired secret fails every
  sign-in.
- **`email` and `xms_edov`** as optional ID-token claims. Entra does not always
  put an email in the ID token, and Keera refuses an identity without one.
  Entra sends no `email_verified`; `xms_edov` says whether the tenant owns the
  address's domain, and Keera reads it instead. If the list does not offer it,
  add it under `optionalClaims.idToken` in the **Manifest**.
- **App roles, not groups**, if the directory should decide roles. App roles
  arrive by name in the `roles` claim and are never replaced by an overage
  pointer. Group object IDs also work (put the GUIDs in the group lists and keep
  the claim as `groups`), but read
  [What each provider does and does not give you](#what-each-provider-does-and-does-not-give-you)
  first. To assign administrators in Keera instead, create no app roles and
  leave out `KEERA_OIDC_ENTRA_ADMIN_GROUPS`.
- **The issuer** must contain the tenant ID and end in `/v2.0`. The v1 endpoint
  issues tokens whose `iss` does not match what discovery reports.

Entra allows an `http` redirect URI only for `localhost`, like Google.

> **Not tested against a real tenant.** This setup follows the protocol and
> Entra's documentation. The group-overage refusal has a test.

## GitHub

GitHub speaks OAuth, not OpenID Connect: it has no discovery and no id_token.
Keera recognises the issuer `https://github.com` and asks GitHub's API who
signed in instead. GitHub Enterprise Server is not supported. The steps and
settings are in
[Set up a provider](https://docs.keera.ch/operate/sign-on/#set-up-a-provider).

**An OAuth App, not a GitHub App.** Keera keeps its token to ask GitHub again,
and a GitHub App's token expires after 8 hours. Register it on your
organisation rather than your account. An OAuth App has one callback URL, so
register a second app for `http://localhost:8080`.

**Any GitHub account can sign in**, as with Google's **External**. So the
gateway refuses to start without `KEERA_OIDC_<NAME>_DOMAINS`. Name your own
domains, or use `*` only where people [sign up](#letting-people-sign-up).

**The address** is the person's primary one if GitHub verified it and the
provider may vouch for its domain. Otherwise it is the first other verified
address that fits, so someone with a private primary address can sign in with
their work one. The `noreply` address is never used. The subject is the
numeric account ID, which survives a renamed login.

**Scopes.** Keera asks for `user:email`, and adds `read:org` when a group list
is set, to read [organisations and
teams](#what-each-provider-does-and-does-not-give-you):

```sh
KEERA_OIDC_GITHUB_ADMIN_GROUPS=bespinian/keera-admins
```

**Asking again.** GitHub issues no refresh token, and an OAuth App's token does
not expire, so Keera keeps that token instead. The person is refused once they
revoke Keera under **Settings → Applications**, or once the account has no
verified address Keera may use.

## LDAP and Active Directory

A directory without OpenID Connect, such as Active Directory or OpenLDAP, can
sign people in directly. An `ldaps://` or `ldap://` issuer makes a provider one.
The panel then asks for a username and password instead of sending the browser
away, and the gateway checks them against the directory. An example is in
[Set up a provider](https://docs.keera.ch/operate/sign-on/#set-up-a-provider).

Separate several groups with `;`, such as
`CN=keera-admins,OU=Groups,...;CN=platform,OU=Groups,...`.

These are the settings an LDAP directory has on top of the common ones, each
`KEERA_OIDC_<NAME>_LDAP_*`:

| Variable                 | Default                 | What it is                                                                                     |
| ------------------------ | ----------------------- | ---------------------------------------------------------------------------------------------- |
| `_LDAP_BIND_DN`          | required                | The service account's DN.                                                                      |
| `_LDAP_BIND_PASSWORD`    | required                | Its password.                                                                                  |
| `_LDAP_BASE_DN`          | required                | Where to search for people. Checked at start-up.                                               |
| `_LDAP_USER_FILTER`      | by directory, see below | Who may sign in.                                                                               |
| `_LDAP_LOGIN_ATTRIBUTES` | by directory, see below | What a person may type as their username.                                                      |
| `_LDAP_CA_FILE`          | the system's roots      | A PEM file of the CAs to trust. It is a path in the gateway's container, so mount the file in. |

**A service account** finds people and reads them, so it needs read access to
users and their `memberOf`. No client ID, client secret or redirect URI is
needed. The gateway binds as the service account at start-up, so a wrong
address or password stops the start.

**The connection is always encrypted.** `ldaps://` uses TLS from the start,
and `ldap://` uses StartTLS before anything else. Plaintext is never used,
since the connection carries passwords. Set `LDAP_CA_FILE` if the directory's
certificate is from your own CA.

**Active Directory is detected** from the server's root entry, which changes
the defaults:

| Setting                  | Active Directory                               | OpenLDAP and others           |
| ------------------------ | ---------------------------------------------- | ----------------------------- |
| `_LDAP_USER_FILTER`      | `(&(objectCategory=person)(objectClass=user))` | `(objectClass=inetOrgPerson)` |
| `_LDAP_LOGIN_ATTRIBUTES` | `sAMAccountName,userPrincipalName,mail`        | `uid,mail`                    |
| The subject              | `objectGUID`                                   | `entryUUID`                   |

`_LDAP_USER_FILTER` decides who may sign in. Add a group to limit it, such as
`(&(objectCategory=person)(objectClass=user)(memberOf=CN=keera-users,OU=Groups,DC=corp,DC=example,DC=ch))`.
`_LDAP_LOGIN_ATTRIBUTES` are what a person may type as their username.

**The address** is the `mail` attribute, and an entry without one is refused.
The name is `displayName`, or `givenName` and `sn`, or `cn`.

**Groups are DNs** from `memberOf`, or from the attribute
`KEERA_OIDC_<NAME>_GROUPS_CLAIM` names. Write the full DN in `ADMIN_GROUPS` and
`OPERATOR_GROUPS`, and separate several with `;`. Case and spaces do not matter. Nested groups are not
followed: a person must be a direct member. On OpenLDAP, `memberOf` needs the
`memberof` overlay.

**`DOMAINS` is required**, and `*` is not allowed: the directory's
administrator sets every address, as on Entra. `SIGNUP` is not allowed either.

**Asking again.** There is no refresh token, so every 15 minutes the service
account reads the person again by their subject, which survives a rename or a
move to another OU. If the entry is gone, no longer matches the user filter, or
is a disabled Active Directory account, their sign-ins end. A sign-in lasts
30 days, as with a refresh token. See
[Asking the directory again](#asking-the-directory-again).

**Wrong passwords.** A wrong password, an unknown username and a locked
account all get the same answer. The reason is in the gateway's log, without
what was typed, so a password typed into the wrong field does not end up there.
Each account gets 5 attempts a minute through Keera, from any number of
addresses and under any of its usernames, on top of the limit per address. Each
replica counts on its own, as for
[the rest of the sign-in throttle](gateway.md#rate-limits-across-replicas).
Set the directory's own lockout policy as usual.

**Passwords pass through the gateway.** Keera never stores or logs them, but
it does see them. If that is not acceptable, put an OpenID Connect provider
with an LDAP connector in front of the directory, such as Keycloak or Dex, and
configure that as an ordinary provider instead.

> **Not tested against a real Active Directory.** This follows the protocol and
> Microsoft's documentation, and is tested against a fake directory.

## Several directories at once

This is the hosted deployment. List the providers, then give each its own
settings:

```sh
KEERA_OIDC_PROVIDERS=google,entra

KEERA_OIDC_GOOGLE_ISSUER=https://accounts.google.com
KEERA_OIDC_GOOGLE_CLIENT_ID=<from the Google console>
KEERA_OIDC_GOOGLE_CLIENT_SECRET=<from the Google console>
KEERA_OIDC_GOOGLE_DOMAINS=*

KEERA_OIDC_ENTRA_ISSUER=https://login.microsoftonline.com/<tenant-id>/v2.0
KEERA_OIDC_ENTRA_CLIENT_ID=<from the Entra admin center>
KEERA_OIDC_ENTRA_CLIENT_SECRET=<from the Entra admin center>
KEERA_OIDC_ENTRA_GROUPS_CLAIM=roles
KEERA_OIDC_ENTRA_DOMAINS=anotherbank.ch

KEERA_PUBLIC_URL=https://keera.example.ch
KEERA_OPERATORS=you@example.ch
```

Nothing is shared between providers. In particular, a customer's directory admin
can create a group of any name, so a shared operator group would let every
customer make operators.

**Each provider says which email domains it may vouch for.** The domain picks
the organisation, and an address can name an operator. A customer's directory
admin can give any user any address, including another customer's or yours. So
`KEERA_OIDC_<NAME>_DOMAINS` lists the domains that provider may sign people in
with, and the gateway refuses to start with several providers if one has none. A
sign-in with an address outside the list is refused.

`*` allows every domain. Use it only for a provider that proves each address
itself, such as Google. Never use it for Entra, where the tenant admin sets the
address.

The example sets no admin groups: they would decide roles for the whole
gateway, and Google users, who [send no
groups](#what-each-provider-does-and-does-not-give-you), could never become
administrators.

**One callback serves every provider.** Keera recognises a sign-in by its state,
not by the callback address. Each provider's client registration must still list
`<KEERA_PUBLIC_URL>/control/auth/callback`.

A **name** matches `[a-z0-9][a-z0-9-]*`. `keera` and `passkey` are reserved for
the gateway's own sign-ins. Do not rename a provider: the name is part of the
sign-in URL and of every identity it creates, so renaming it cuts off everyone
who signed in through it. The button label comes from the name, or from
`KEERA_OIDC_<NAME>_LABEL`.

### One person, one provider

An identity is stored as `<provider>:<subject>`, because a subject is only
unique within its directory.

A person keeps the provider they first signed in with, in every organisation. A
sign-in with the same address through another provider is refused, and the error
names the one to use. Otherwise anyone who gets the same address, through a
weaker directory, could become them. Two identities that already shared an
address before this rule keep working. A passkey account is its own identity, in
one organisation.

A row an administrator created in advance has no subject yet. The person's
identity takes it over when they first land in that row's organisation. A row
that already has _another_ subject, from another provider or the same one, is
refused instead.

## Which organisation a sign-in lands in

`orgFor` in `internal/control/auth.go` checks in this order:

1. The organisations the person is already in: the one this browser was last in,
   else the one they used last. See
   [Several organisations](gateway.md#several-organisations).
2. An organisation whose `email_domain` matches the address's domain. With
   several providers, only a provider allowed that domain gets this far.
3. With sign-up on that provider, the sign-up screen. See
   [Letting people sign up](#letting-people-sign-up).
4. The only organisation, if there is exactly one. Skipped when any provider
   allows sign-up.
5. Otherwise refuse.

On a compose deployment with one organisation, step 4 applies. So **every**
directory user who passes the consent screen joins that organisation with the
default role. Restrict the consent screen to your own directory to keep that to
your own staff.

**On the hosted deployment, avoid step 4.** It stops applying once there is a
second organisation, but until then a new customer's first sign-in lands in the
existing organisation. Give every organisation its domain, the one that was
there first included, before you create the second one; see
[Organisations and domains](https://docs.keera.ch/operate/sign-on/#organisations-and-domains).
Domains are unique: a second organisation claiming the same one is refused.

Setting a domain moves nobody. It is only read on a first sign-in, so existing
users stay where they are. `keera org set <org> --no-domain` removes it.

## Letting people sign up

By default, somebody whose sign-in matches no organisation is refused, and an
operator creates one for them. On a public deployment, people can do this
themselves:

```sh
KEERA_OIDC_GOOGLE_SIGNUP=true
```

Set it only on a provider that proves every address itself, such as Google with
`KEERA_OIDC_GOOGLE_DOMAINS=*`. An [LDAP directory](#ldap-and-active-directory)
cannot. If such a provider vouches for any domain, a
sign-in without `email_verified: true` (Entra: `xms_edov`) is refused. Passkeys
never sign anybody up. The gateway refuses to start if any provider also sets
`ADMIN_GROUPS`: that group decides the administrator role for the whole gateway,
so the person who created an organisation would lose the role at their next
sign-in. It also refuses to start until `KEERA_UPSTREAM_PRIVATE` says what
inside your network an organisation may reach. See
[install.md](install.md#where-the-gateway-may-connect).

A sign-in that matches no organisation then lands on a screen to create an
organisation or join an invitation
([Quickstart](https://docs.keera.ch/get-started/quickstart/#1-sign-in)). If the
name is taken, the person's address is added to the name, so a stranger cannot
find out which customers you have. An invitation is never joined without asking: an
administrator of an organisation without a domain can add any address.

The new organisation gets no email domain. Set one with `keera org set` once you
know the customer. `keera login` cannot sign up: it says to open the panel
first.

### What a new organisation can use

Anyone with a Google account can now be an administrator. So an organisation
made this way starts **limited**, and gets nothing that costs you money or
reaches your network:

| Works at once                                                   | Waits                                               |
| --------------------------------------------------------------- | --------------------------------------------------- |
| Models on your provider keys, paid from credit in advance       | Models inside your network, such as Keera Engine    |
| Models and MCP servers on public addresses, with their own keys | MCP servers inside your network                     |
|                                                                 | Sandboxes                                           |
|                                                                 | Models on your provider keys, without card payments |

A request for something that waits is refused with `org_limited`, and
`/api/v1/models` does not list it. A banner on every screen names what waits.
With card payments, while the panel's first-run checklist asks for credit (see
[install.md](install.md#first-run)), the checklist says it instead. The gateway also checks the
address it connects to, so a public name that points into your network does
not get through either.

The limit lifts with the organisation's first payment (see
[billing.md](billing.md)). After that, it reaches only the hosts inside your
network that `KEERA_UPSTREAM_PRIVATE` names. Credit an operator grants does not
lift it. An operator can also lift it at any time:

```sh
keera org set <org> --lift-limit
```

On the panel, that is **Organisations → Lift limit**. `keera org list` marks
limited organisations. An operator can set an organisation back to limited with
`PATCH /control/v1/orgs/{id}` and `{"limited": true}`.

## Signing in

An LDAP directory's button opens a username and password form. When that
directory is the only way to sign in, the form is open from the start.

Signing out also sends the browser to the provider's sign-out page, if the
provider has one.

The panel and `keera user list` show each person's name next to their address.
It comes from the `name` claim, or from `given_name` and `family_name`, so it
needs the `profile` scope. Every sign-in updates it. Someone who has never
signed in through a provider shows only their address.

A sign-in through a provider's own page must finish in the browser that started
it. The panel sets a short-lived cookie with the sign-in's state, and the
callback refuses a state the browser does not hold. So a link to someone else's
callback cannot sign you in to their account. Start from `KEERA_PUBLIC_URL`, since the callback goes
there and the cookie has to be on that address.

## Signing in from the command line

`keera login` is the same sign-in for the terminal; see
[Sign in](https://docs.keera.ch/use/sign-in/#in-the-terminal) for how to use
it. It opens a browser, the directory signs the person in as for the panel, and
the command gets a credential for that person. Commands then run as them: their
role decides what is allowed, `keera usage` shows their organisation, and the
audit log records their address.

Where the credential is kept, how a command picks a gateway, and why
`KEERA_OPERATOR_KEY` wins over it:
[The `keera` command](install.md#the-keera-command).

The credential lasts 30 days, or 12 hours where the provider gives Keera no
refresh token (see [Asking the directory again](#asking-the-directory-again)).

### What it does, and what makes that safe

It uses the loopback redirect from RFC 8252, like `gcloud` and `gh`:

1. `keera login` listens on a port on 127.0.0.1 and opens `/control/auth/login`
   with that address, a random state, and the SHA-256 of a proof key it keeps.
2. The identity provider signs the person in as usual. No new redirect URI is
   registered: the panel's callback is reused.
3. The callback redirects the browser to the loopback port with a one-time code.
4. `keera login` redeems the code, with the proof key, for a token.

Two rules make it safe:

- The redirect must be a literal loopback address (such as `127.0.0.1` or
  `[::1]`, with a port, over `http`), or the sign-in is refused. The name
  `localhost` is refused too, because it can resolve to anything.
- The code is useless without the proof key. Any process on the machine can bind
  a loopback port, but only the process that started the sign-in has the proof
  key. The code is deleted when redeemed.

A role change signs out the terminal as well as the browser.

### On a machine with no browser

The sign-in URL is printed as well as opened, so on a host without a desktop you
can copy and paste it. `--no-browser` skips opening the browser.

**Over ssh, copy and paste is not enough.** The loopback listener is on the
remote machine and the browser is on yours. Forward the port
([Sign in](https://docs.keera.ch/use/sign-in/#in-the-terminal)), or sign in on
a machine with a browser and copy the credentials file across.

## Signing in to Keera Chat

Keera Chat is a chat in the browser
for the people behind a gateway. It signs them in the way `keera login` does,
with its server in the place of the terminal: the person signs in here, and
Keera Chat gets a credential that acts as them. Their messages then go out with
their own keys, under the same guardrails, budgets and audit log.

Keera Chat runs on its own address, which is not a loopback port, so the gateway
has to be told it in `KEERA_CHAT_URL`
([Keera Chat](https://docs.keera.ch/operate/chat/)). It is an origin only: a
path, query or fragment stops the start.

The gateway then also hands sign-ins to `https://chat.example.ch/auth/callback`,
that address exactly, with the same one-time code and proof key as for the
command line. Unset, only loopback addresses are allowed, which is enough for a
Keera Chat on `http://127.0.0.1` during development.

Keera Chat's requests show as **Keera Chat** under **My clients** and on the
map, and the panel's account menu links to it.

## Moving off localhost

Put the gateway's port behind a TLS terminator, then change two things: the
authorised redirect URI in each provider's console, and `KEERA_PUBLIC_URL`.

[`KEERA_SECURE_COOKIES`](install.md#everything-else) follows the scheme, so the
session cookie becomes `Secure` without another setting.

## Asking the directory again

A sign-in lasts 30 days, in the browser and with `keera login`. A sign-in to
the panel with the operator key lasts 12 hours, since no directory checks it.
So Keera does not only read the directory at sign-in: it keeps the refresh token the sign-in
returns, encrypted with `KEERA_SECRET_KEY`, and spends it every 15 minutes while
the person is active.

- If the directory refuses, because the person was removed or disabled or
  revoked Keera's access, all their sessions and `keera login` tokens end, in
  every organisation they are in.
- If it answers, the role and the name are applied again, as at sign-in, in
  every organisation the person is in.
- If it cannot be reached, the person stays signed in, and the next request asks
  again 15 minutes later.
- If the stored refresh token cannot be decrypted, usually because
  `KEERA_SECRET_KEY` [changed](install.md#changing-the-secret-key), the person is
  signed out, as if the directory had refused. Signing in again stores a new one.

When `KEERA_OIDC_<NAME>_SCOPES` is unset, Keera asks for `offline_access`, which
is what gets a refresh token from most providers. On Google it asks for offline
access instead, as Google expects. GitHub [works
differently](#github), and so does [LDAP](#ldap-and-active-directory). If your provider refuses `offline_access`,
set the scopes without it. Without a refresh token, the directory is only read
at sign-in, and a sign-in lasts 12 hours instead of 30 days, in the browser
and with `keera login`.

The refresh token is deleted once the person is disabled or removed in every
organisation they were in.

## When someone leaves

Removing a person from the directory ends their sessions and `keera login`
tokens ([Asking the directory again](#asking-the-directory-again)), but not
their API keys. The directory never sees
those. So disable them in Keera as well; see
[When someone leaves](https://docs.keera.ch/manage/users/#when-someone-leaves).
It acts in one organisation: someone in several is disabled in each.
Immediately:

- they cannot sign in, and all their sessions and `keera login` tokens end
- every key attributed to them is revoked for good
- their agent sandboxes, and any sandbox that has not started yet, are
  terminated. Their other sandboxes are suspended with the volume kept. An
  administrator can keep or terminate them; getting at the work inside takes an
  operator, since nobody else can open a shell in another person's sandbox
- the repository credentials those sandboxes held are revoked

The panel hides disabled people until you tick **Show disabled**. An
administrator can disable anyone in
their organisation except themselves. Nobody can disable an operator this way;
take them out of `KEERA_OPERATORS` or the operator group instead.

Keys attributed to nobody, such as a build pipeline's, are not affected. Give a
key a `--user` when a person is behind it.

## Passkeys

Passkeys are off by default; `KEERA_PASSKEYS=true` turns them on. What they
are for: [Passkeys](https://docs.keera.ch/operate/sign-on/#passkeys).

A passkey belongs to the host name in `KEERA_PUBLIC_URL`. Browsers allow
passkeys only on https, or on `http://localhost` and `*.localhost`, and never on
an IP address, so the gateway refuses to start otherwise. **Do not change the
host name later**: every passkey stops working, and each person needs a new
set-up link.

### Adding someone

An administrator creates the account and gets a one-time set-up link; see
[Add a person](https://docs.keera.ch/manage/users/#add-a-person). Whoever opens
the link can add a passkey to that account. It works once and for 24 hours, and
a new link replaces the last one. Opening it creates the passkey and signs the
person in.

Adding a passkey for another device needs a browser sign-in from the last 15
minutes, so a stolen session cannot plant a passkey of its own. A person can
remove one, but not their last. Removing someone else's passkey also signs them
out everywhere.

A person who was added but has not signed in yet can be moved to passkeys with
`keera user passkey-link`, or **Use a passkey** on the panel.

### What keeps passkeys from weakening single sign-on

- **A directory account never gets a passkey.** Leaving the directory has to
  keep locking a person out, so `passkey-link` refuses an account linked to an
  identity provider. In the other direction, a passkey account is never taken
  over by a first SSO sign-in with the same address.
- **Leaving is handled in Keera.** No directory knows about a passkey account,
  so disable it when the person leaves: `keera user disable`. That also removes
  their passkeys. `keera user enable` lets them back in only with a new link.
- **A passkey account is never an operator.** Nobody proved its address; an
  administrator typed it. So `KEERA_OPERATORS` does not apply to it, and a
  passkey sign-in drops the operator role if the row somehow has it. Operators
  sign in through a directory, or with the operator key.
- **An administrator can only use their own domain.** If the organisation has an
  email domain, an administrator can only create passkey accounts at it. An
  operator can use any address.
- **Admin groups still decide.** Where `KEERA_OIDC_<NAME>_ADMIN_GROUPS` is set,
  a passkey account can only be a member, as no directory group speaks for it.

Each sign-in checks that the passkey verified the person (PIN or biometrics),
that the page asking was this gateway's own, and that the passkey's counter did
not go backwards. The last one catches a copied hardware key. Synced passkeys
always report zero, which is fine.

### With no identity provider

With passkeys and no directory, the sign-in screen shows **Sign in with a
passkey** and, below it, the operator key. The operator key stays on the screen
because a passkey account is never an operator: the first administrator is made
with the operator key. On such a gateway, `keera login` needs no `--provider`.

## When it does not work

| Symptom                                                        | Cause                                                                                                                                                                       |
| -------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Gateway will not start, "discovering the identity provider"    | An issuer is wrong, or the container cannot reach it. The message names which one.                                                                                          |
| Gateway will not start, "connecting to the … directory"        | The LDAP address, bind DN or password is wrong, or the certificate is not trusted (`_LDAP_CA_FILE`).                                                                        |
| Gateway will not start, "reading the base DN"                  | `_LDAP_BASE_DN` does not exist, or the service account cannot read it.                                                                                                      |
| Gateway will not start, "do not make an LDAP filter"           | `_LDAP_USER_FILTER` or `_LDAP_LOGIN_ATTRIBUTES` has a mistake, such as a parenthesis too many.                                                                              |
| "that username and password were not accepted"                 | A wrong password, an unknown username, a locked or disabled account, or one the user filter leaves out. The gateway log has the directory's reason, without what was typed. |
| "too many sign-in attempts for this account"                   | 5 attempts a minute per account. Wait a minute.                                                                                                                             |
| "the directory did not answer"                                 | The gateway cannot reach the LDAP directory, or the directory refused the search, such as when the service account may no longer read. The gateway log has the error.       |
| "this directory account cannot be used to sign in to Keera"    | The entry has no `mail` or no subject attribute. The gateway log names the entry.                                                                                           |
| "a sign-in has to name one of…"                                | Several ways to sign in are configured, passkeys included, and the link named none. Start from the panel, not the URL.                                                      |
| "started in another browser or tab"                            | The callback came to a browser that did not start the sign-in, or to another address than the panel's. Start again from `KEERA_PUBLIC_URL`.                                 |
| Signed in, but everything is read-only                         | You have the default role. Ask an administrator for `keera user role <you> admin`, or, if the provider has admin groups, to be added to one.                                |
| The panel will not let anybody change a role                   | `KEERA_OIDC_<NAME>_ADMIN_GROUPS` is set on some provider, so the directory decides. Change the group, or unset it on every provider.                                        |
| The panel refuses the operator role                            | It always does. `KEERA_OPERATORS` or `KEERA_OIDC_<NAME>_OPERATOR_GROUPS` grants it.                                                                                         |
| "that email domain belongs to another organisation"            | Each domain belongs to one organisation. `keera org list` shows which; clear it there first.                                                                                |
| "cannot be used with … sign-in"                                | The provider is only allowed some domains (`KEERA_OIDC_<NAME>_DOMAINS`), and the address is at another. Sign in through its own provider.                                   |
| "is not a verified address"                                    | `email_verified` (Entra: `xms_edov`) is false. Verify the email in the directory. A missing claim is accepted, except on a sign-up provider for any domain.                 |
| "in too many groups"                                           | Entra group overage. Use app roles. See above.                                                                                                                              |
| "signs in through …, the way it first signed in"               | That address signed in first through another provider. Use that one. See [One person, one provider](#one-person-one-provider).                                              |
| "already belongs to an account linked to a different identity" | The address belongs to someone else's account in this organisation. An operator can move it.                                                                                |
| Entra: "returned no email claim"                               | Add `email` as an optional ID-token claim on the app registration.                                                                                                          |
| `keera login`: "has no identity provider configured"           | The gateway has none. Use `KEERA_OPERATOR_KEY`, or configure one as above.                                                                                                  |
| `keera login`: "has to come back to a loopback address"        | The command line is older than the gateway, or something rewrote its redirect. Rebuild it.                                                                                  |
| `keera login`: the browser signs in and nothing happens        | The browser is not on the same machine as the command, usually because of ssh. See above.                                                                                   |
| A command says "your sign-in is no longer valid"               | It expired, a role change ended it, or someone ran `keera logout --all`. Run `keera login` again.                                                                           |
| Entra: "did not match the issuer URL returned by provider"     | The issuer is `.../common/v2.0`, which answers discovery with a templated `{tenantid}` that matches nothing. Use the tenant ID.                                             |
| Boot fails with "KEERA_PASSKEYS: passkeys need …"              | `KEERA_PUBLIC_URL` is unset, plain http on a name other than `localhost` or `*.localhost`, or an IP address. Browsers refuse passkeys there.                                |
| "this set-up link has expired, was already used, or …"         | Links work once, for 24 hours, and a new one replaces the last. Ask for a new one: `keera user passkey-link <email>`.                                                       |
| "that passkey was not accepted"                                | The passkey was removed, belongs to a disabled person, or is for another host name. The gateway log says which.                                                             |
| The browser says the passkey prompt failed with SecurityError  | The panel is open at an address other than `KEERA_PUBLIC_URL`, such as `127.0.0.1` instead of `localhost`.                                                                  |
